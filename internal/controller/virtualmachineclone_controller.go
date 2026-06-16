/*
Copyright 2026.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/clone"
	"github.com/ruddervirt/aileron/internal/namespace"
	"github.com/ruddervirt/aileron/internal/network"
)

const cloneFinalizerName = "ruddervirt.io/clone-finalizer"

// +kubebuilder:rbac:groups=ruddervirt.io,resources=virtualmachineclones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ruddervirt.io,resources=virtualmachineclones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ruddervirt.io,resources=virtualmachineclones/finalizers,verbs=update
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots;volumesnapshotcontents;volumesnapshotclasses,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubeovn.io,resources=vpcs;subnets,verbs=get;list;watch;create;delete;update;patch
// +kubebuilder:rbac:groups=kubeovn.io,resources=vpc-egress-gateways,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;delete

// VirtualMachineCloneReconciler reconciles VirtualMachineClone objects.
type VirtualMachineCloneReconciler struct {
	Client     client.Client
	Scheme     *runtime.Scheme
	RESTConfig *rest.Config

	snapshotManager clone.SnapshotManager
	volumeManager   clone.VolumeManager
}

func (r *VirtualMachineCloneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	vmClone := &v1alpha1.VirtualMachineClone{}
	if err := r.Client.Get(ctx, req.NamespacedName, vmClone); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion.
	if !vmClone.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, vmClone)
	}

	// Ensure finalizer — use Patch to avoid resourceVersion conflicts.
	if !controllerutil.ContainsFinalizer(vmClone, cloneFinalizerName) {
		finalizerPatch := client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"finalizers":["`+cloneFinalizerName+`"]}}`))
		if err := r.Client.Patch(ctx, vmClone, finalizerPatch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize phase.
	if vmClone.Status.Phase == "" {
		vmClone.Status.Phase = v1alpha1.ClonePhasePending
		now := metav1.Now()
		vmClone.Status.StartTime = &now
	}

	// Check timeout.
	if vmClone.Status.Phase != v1alpha1.ClonePhaseReady && vmClone.Status.Phase != v1alpha1.ClonePhaseFailed {
		timeout := 15 * time.Minute
		if vmClone.Spec.Timeout != nil {
			timeout = vmClone.Spec.Timeout.Duration
		}
		if vmClone.Status.StartTime != nil && time.Since(vmClone.Status.StartTime.Time) > timeout {
			return r.failClone(ctx, vmClone, fmt.Errorf("clone timed out after %s", timeout))
		}
	}

	var result ctrl.Result
	var err error

	switch vmClone.Status.Phase {
	case v1alpha1.ClonePhasePending:
		result, err = r.handlePending(ctx, vmClone)
	case v1alpha1.ClonePhaseValidating:
		result, err = r.handleValidating(ctx, vmClone)
	case v1alpha1.ClonePhaseSnapshotSelection:
		result, err = r.handleSnapshotSelection(ctx, vmClone)
	case v1alpha1.ClonePhaseVolumeProvisioning:
		result, err = r.handleVolumeProvisioning(ctx, vmClone)
	case v1alpha1.ClonePhaseNetworking:
		result, err = r.handleNetworking(ctx, vmClone)
	case v1alpha1.ClonePhaseVMProvisioning:
		result, err = r.handleVMProvisioning(ctx, vmClone)
	case v1alpha1.ClonePhaseReady:
		return r.handlePowerManagement(ctx, vmClone)
	case v1alpha1.ClonePhaseFailed:
		return ctrl.Result{}, nil
	default:
		return r.failClone(ctx, vmClone, fmt.Errorf("unknown phase %q", vmClone.Status.Phase))
	}

	if err != nil {
		logger.Error(err, "Error reconciling clone", "phase", vmClone.Status.Phase)
	}
	return result, err
}

func (r *VirtualMachineCloneReconciler) handlePending(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (ctrl.Result, error) {
	// Generate unique clone ID for resource naming.
	if vmClone.Status.CloneID == "" {
		vmClone.Status.CloneID = namespace.GenerateNamespaceName("ns-")
	}
	vmClone.Status.CloneNamespace = OperatorNamespace()

	if vmClone.Status.AgeAnchor == nil {
		r.resolveAgeAnchor(ctx, vmClone)
	}

	cloneID := vmClone.Status.CloneID
	vmns := &v1alpha1.VirtualMachineNamespace{}
	vmns.Name = cloneID
	vmns.Namespace = OperatorNamespace()
	vmns.Spec = v1alpha1.VirtualMachineNamespaceSpec{
		OwnerRef: &v1alpha1.VirtualMachineNamespaceOwnerRef{
			Kind:      "VirtualMachineClone",
			Name:      vmClone.Name,
			Namespace: vmClone.Namespace,
		},
	}
	vmns.Labels = map[string]string{
		"ruddervirt.io/clone": cloneID,
	}
	if err := r.Client.Create(ctx, vmns); err != nil && !errors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("creating VirtualMachineNamespace: %w", err)
	}

	vmClone.Status.Phase = v1alpha1.ClonePhaseValidating
	vmClone.Status.Message = "validating template"
	if err := r.Client.Status().Update(ctx, vmClone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *VirtualMachineCloneReconciler) handleValidating(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (ctrl.Result, error) {
	templateNS, templateVMs, err := clone.ValidateTemplate(ctx, r.Client, vmClone.Spec.TemplateName)
	if err != nil {
		return r.failClone(ctx, vmClone, err)
	}

	vmClone.Status.TemplateNamespace = templateNS
	meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.CloneConditionTemplateValidated,
		Status:  metav1.ConditionTrue,
		Reason:  "Validated",
		Message: fmt.Sprintf("template namespace %s has %d VMs", templateNS, len(templateVMs)),
	})

	vmClone.Status.Phase = v1alpha1.ClonePhaseSnapshotSelection
	vmClone.Status.Message = fmt.Sprintf("validated template with %d VMs", len(templateVMs))
	if err := r.Client.Status().Update(ctx, vmClone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *VirtualMachineCloneReconciler) handleSnapshotSelection(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (ctrl.Result, error) {
	templateNS := vmClone.Status.TemplateNamespace
	cloneID := vmClone.Status.CloneID

	// Build initial volume states if not yet done.
	if len(vmClone.Status.VolumeStates) == 0 {
		templateVMs, err := clone.ListTemplateVMs(ctx, r.Client, templateNS, vmClone.Spec.TemplateName)
		if err != nil {
			return ctrl.Result{}, err
		}
		states, err := r.snapshotManager.BuildInitialVolumeStates(ctx, templateVMs, templateNS)
		if err != nil {
			return ctrl.Result{}, err
		}
		vmClone.Status.VolumeStates = states
		if len(states) == 0 {
			vmClone.Status.Phase = v1alpha1.ClonePhaseNetworking
			vmClone.Status.Message = "no volumes require cloning"
			if err := r.Client.Status().Update(ctx, vmClone); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Ensure snapshots for all volumes.
	allReady := true
	for i := range vmClone.Status.VolumeStates {
		state := &vmClone.Status.VolumeStates[i]
		ready, err := r.snapshotManager.EnsureBaseSnapshotReady(ctx, cloneID, state, templateNS)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("snapshot for volume %s: %w", state.VolumeName, err)
		}
		if !ready {
			allReady = false
		}
	}

	if !allReady {
		vmClone.Status.Message = "waiting for snapshots to become ready"
		if err := r.Client.Status().Update(ctx, vmClone); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.CloneConditionSnapshotSelected,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "all snapshots ready",
	})
	vmClone.Status.Phase = v1alpha1.ClonePhaseVolumeProvisioning
	vmClone.Status.Message = "snapshots ready, provisioning volumes"
	if err := r.Client.Status().Update(ctx, vmClone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *VirtualMachineCloneReconciler) handleVolumeProvisioning(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (ctrl.Result, error) {
	cloneNS := vmClone.Status.CloneNamespace
	cloneID := vmClone.Status.CloneID

	allReady := true
	for i := range vmClone.Status.VolumeStates {
		state := &vmClone.Status.VolumeStates[i]

		if state.Phase == v1alpha1.CloneVolumePhaseComplete || state.Phase == v1alpha1.CloneVolumePhasePVCBound {
			continue
		}

		ready, err := r.volumeManager.EnsureClonePVC(ctx, cloneID, state, cloneNS)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("clone PVC for %s: %w", state.VolumeName, err)
		}
		if !ready {
			allReady = false
		}
	}

	if !allReady {
		vmClone.Status.Message = "provisioning volumes"
		if err := r.Client.Status().Update(ctx, vmClone); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.CloneConditionVolumesReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "all volumes provisioned",
	})
	vmClone.Status.Phase = v1alpha1.ClonePhaseNetworking
	vmClone.Status.Message = "volumes ready, configuring network"
	if err := r.Client.Status().Update(ctx, vmClone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

//nolint:gocyclo
func (r *VirtualMachineCloneReconciler) handleNetworking(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	cloneNS := vmClone.Status.CloneNamespace

	// Derive network topology: explicit spec takes priority, then template annotation.
	var vpcs []v1alpha1.VPC
	var subnets []v1alpha1.Subnet

	if vmClone.Spec.Network != nil {
		vpcs = vmClone.Spec.Network.VPCs
		subnets = vmClone.Spec.Network.Subnets
	} else {
		// Auto-derive from template VM annotations.
		topo, err := r.extractTopoFromTemplate(ctx, vmClone)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("extracting network topology: %w", err)
		}
		if topo == nil {
			vmClone.Status.Phase = v1alpha1.ClonePhaseVMProvisioning
			vmClone.Status.Message = "no network topology, provisioning VMs"
			if err := r.Client.Status().Update(ctx, vmClone); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		for _, v := range topo.VPCs {
			vpcs = append(vpcs, v1alpha1.VPC{Name: v.Name, Internet: v.Internet})
		}
		for _, s := range topo.Subnets {
			subnets = append(subnets, v1alpha1.Subnet{
				Name:      s.Name,
				VPC:       s.VPC,
				CIDR:      s.CIDR,
				DHCP:      s.DHCP,
				DNS:       s.DNS,
				Unmanaged: s.Unmanaged,
			})
		}
		if vmClone.Status.Network == nil {
			logger.Info("Derived network topology from template", "vpcs", len(vpcs), "subnets", len(subnets))
		}
	}

	if len(vpcs) == 0 && len(subnets) == 0 {
		vmClone.Status.Phase = v1alpha1.ClonePhaseVMProvisioning
		vmClone.Status.Message = "no network configuration, provisioning VMs"
		if err := r.Client.Status().Update(ctx, vmClone); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	cloneID := vmClone.Status.CloneID
	labels := map[string]string{
		"ruddervirt.io/clone": cloneID,
	}

	// Ensure VPCs.
	var vpcsCreated []string
	for _, vpcSpec := range vpcs {
		vpcName := network.VPCName(vmClone.Status.CloneID, vpcSpec.Name)
		vpcInternet := vpcSpec.Internet
		if err := network.EnsureVPC(ctx, r.Client, vpcName, cloneNS, vpcInternet, labels); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring VPC %s: %w", vpcName, err)
		}
		vpcsCreated = append(vpcsCreated, vpcName)
	}

	// Check VPC readiness before creating subnets.
	for _, vpcName := range vpcsCreated {
		ready, err := network.IsVPCReady(ctx, r.Client, vpcName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			vmClone.Status.Network = &v1alpha1.CloneNetworkStatus{
				VPCsCreated: vpcsCreated,
				Ready:       false,
			}
			vmClone.Status.Message = "waiting for VPCs to be ready"
			if err := r.Client.Status().Update(ctx, vmClone); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// Ensure OVN subnets and their NADs.
	var subnetsCreated []string
	for i, subnetSpec := range subnets {
		subnetName := network.SubnetName(vmClone.Status.CloneID, subnetSpec.Name)
		vpcName := network.VPCName(vmClone.Status.CloneID, subnetSpec.VPC)
		if subnetSpec.VPC == "" && len(vpcsCreated) > 0 {
			vpcName = vpcsCreated[0]
		}
		cidr := subnetSpec.CIDR
		if cidr == "" {
			cidr = fmt.Sprintf("10.0.%d.0/24", i)
		}

		// Find if this subnet's VPC has internet.
		vpcInternet := false
		for _, v := range vpcs {
			if v.Name == subnetSpec.VPC && v.Internet {
				vpcInternet = true
				break
			}
		}

		netSubnetSpec := network.SubnetSpec{
			CIDR: cidr,
			DHCP: subnetSpec.DHCP == nil || *subnetSpec.DHCP,
			DNS:  subnetSpec.DNS,
		}
		// Mirror the build-side unmanaged translation (see internal/build/network.go
		// and SubnetSpec.ApplyUnmanaged): OVN DHCP off, gateway probe skipped, OVN
		// gateway router port relocated off the guest gateway's address.
		if subnetSpec.Unmanaged {
			if err := netSubnetSpec.ApplyUnmanaged(); err != nil {
				return r.failClone(ctx, vmClone, fmt.Errorf("subnet %q: %w", subnetSpec.Name, err))
			}
		}

		if err := network.EnsureSubnet(ctx, r.Client, subnetName, vpcName, cloneNS, netSubnetSpec, vpcInternet, labels); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring subnet %s: %w", subnetName, err)
		}
		subnetsCreated = append(subnetsCreated, subnetName)
	}

	// Check OVN subnet readiness.
	allReady := true
	for _, subnetName := range subnetsCreated {
		ready, err := network.IsSubnetReady(ctx, r.Client, subnetName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			allReady = false
		}
	}

	// Ensure egress gateways for internet-enabled VPCs.
	var egressCreated []string
	if allReady {
		for _, vpcSpec := range vpcs {
			if !vpcSpec.Internet {
				continue
			}
			vpcName := network.VPCName(vmClone.Status.CloneID, vpcSpec.Name)
			gwName := vpcName + "-egress"

			// Skip unmanaged subnets: their entire usable range is excluded
			// from OVN IPAM, so the egress gateway pod can't get an IP.
			var vpcSubnetNames []string
			var firstSubnet string
			for _, s := range subnets {
				if s.VPC != vpcSpec.Name || s.Unmanaged {
					continue
				}
				sn := network.SubnetName(vmClone.Status.CloneID, s.Name)
				vpcSubnetNames = append(vpcSubnetNames, sn)
				if firstSubnet == "" {
					firstSubnet = sn
				}
			}
			if len(vpcSubnetNames) == 0 {
				logger.Info("VPC has internet=true but no managed subnets for egress gateway; skipping", "vpc", vpcSpec.Name)
				continue
			}

			if err := network.EnsureEgressGateway(ctx, r.Client, gwName, cloneNS, vpcName, firstSubnet, vpcSubnetNames, labels); err != nil {
				return ctrl.Result{}, fmt.Errorf("ensuring egress gateway for VPC %s: %w", vpcSpec.Name, err)
			}
			egressCreated = append(egressCreated, gwName)

			ready, internalIP, err := network.IsEgressGatewayReady(ctx, r.Client, gwName, cloneNS)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("checking egress gateway readiness: %w", err)
			}
			if !ready {
				allReady = false
			} else if internalIP != "" {
				if err := network.EnsureVPCDefaultRoute(ctx, r.Client, vpcName, internalIP); err != nil {
					return ctrl.Result{}, fmt.Errorf("setting VPC default route: %w", err)
				}
			}
		}
	}

	vmClone.Status.Network = &v1alpha1.CloneNetworkStatus{
		VPCsCreated:           vpcsCreated,
		SubnetsCreated:        subnetsCreated,
		EgressGatewaysCreated: egressCreated,
		Ready:                 allReady,
	}

	if !allReady {
		vmClone.Status.Message = "waiting for network resources to be ready"
		if err := r.Client.Status().Update(ctx, vmClone); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Register subnet CIDRs in the ovn40subnets ipset for DHCP to work.
	if r.RESTConfig != nil {
		for i, s := range subnets {
			cidr := s.CIDR
			if cidr == "" {
				cidr = fmt.Sprintf("10.0.%d.0/24", i)
			}
			if err := network.EnsureIPSetEntry(ctx, r.Client, r.RESTConfig, cidr); err != nil {
				logger.Error(err, "Failed to add subnet CIDR to ovn40subnets ipset", "cidr", cidr)
			}
		}
	}

	meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.CloneConditionNetworkReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "network resources ready",
	})
	vmClone.Status.Phase = v1alpha1.ClonePhaseVMProvisioning
	vmClone.Status.Message = "network ready, creating VMs"
	if err := r.Client.Status().Update(ctx, vmClone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// extractTopoFromTemplate reads the network topology annotation from the first
// template VM that has one.
func (r *VirtualMachineCloneReconciler) extractTopoFromTemplate(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (*clone.NetworkTopology, error) {
	templateVMs, err := clone.ListTemplateVMs(ctx, r.Client, vmClone.Status.TemplateNamespace, vmClone.Spec.TemplateName)
	if err != nil {
		return nil, err
	}
	for _, vm := range templateVMs {
		topo, err := clone.ExtractNetworkTopology(vm)
		if err != nil {
			return nil, err
		}
		if topo != nil {
			return topo, nil
		}
	}
	return nil, nil
}

func (r *VirtualMachineCloneReconciler) handleVMProvisioning(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (ctrl.Result, error) {
	templateNS := vmClone.Status.TemplateNamespace
	cloneNS := vmClone.Status.CloneNamespace

	templateVMs, err := clone.ListTemplateVMs(ctx, r.Client, templateNS, vmClone.Spec.TemplateName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Extract network topology from template for network rewiring.
	var networkTopo *clone.NetworkTopology
	for _, vm := range templateVMs {
		topo, err := clone.ExtractNetworkTopology(vm)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("extracting network topology: %w", err)
		}
		if topo != nil {
			networkTopo = topo
			break
		}
	}

	// Create VMs.
	source := vmClone.GetAnnotations()[v1alpha1.AnnotationOrigin]
	if err := clone.EnsureVMs(ctx, r.Client, templateVMs, vmClone.Status.CloneID, cloneNS, source, vmClone.Status.VolumeStates, networkTopo, vmClone.Status.AgeAnchor); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating VMs: %w", err)
	}

	// Check VM readiness.
	statuses, allReady, err := clone.CheckVMsReady(ctx, r.Client, templateVMs, vmClone.Status.CloneID, cloneNS)
	if err != nil {
		return ctrl.Result{}, err
	}
	vmClone.Status.VMStatuses = statuses

	if !allReady {
		vmClone.Status.Message = "waiting for VMs to become ready"
		if err := r.Client.Status().Update(ctx, vmClone); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// All ready.
	now := metav1.Now()
	vmClone.Status.CompletionTime = &now
	vmClone.Status.Phase = v1alpha1.ClonePhaseReady
	vmClone.Status.Message = "clone complete"
	vmClone.Status.EgressGatewayReady = true

	meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.CloneConditionVMProvisioned,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "all VMs provisioned",
	})
	meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.CloneConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "clone complete",
	})

	if err := r.Client.Status().Update(ctx, vmClone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *VirtualMachineCloneReconciler) handlePowerManagement(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	cloneID := vmClone.Status.CloneID
	cloneNS := vmClone.Status.CloneNamespace

	// Check if any VMIs are running for this clone.
	vmiList := &unstructured.UnstructuredList{}
	vmiList.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstanceList",
	})
	if err := r.Client.List(ctx, vmiList,
		client.InNamespace(cloneNS),
		client.MatchingLabels{"ruddervirt.io/clone": cloneID},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing clone VMIs: %w", err)
	}
	anyVMRunning := len(vmiList.Items) > 0

	// Egress tracks VM power: up when any VM is running, down when all stopped.
	// User can override via spec.egressGateway.enabled.
	egressEnabled := anyVMRunning
	if vmClone.Spec.EgressGateway != nil && vmClone.Spec.EgressGateway.Enabled != nil {
		egressEnabled = *vmClone.Spec.EgressGateway.Enabled
	}

	changed := false
	if vmClone.Status.Network != nil {
		for _, gwName := range vmClone.Status.Network.EgressGatewaysCreated {
			var replicas int64
			if egressEnabled {
				replicas = 1
			}
			if err := network.ScaleEgressGateway(ctx, r.Client, gwName, cloneNS, replicas); err != nil {
				logger.Error(err, "Failed to scale egress gateway", "gateway", gwName)
			}
		}
		if egressEnabled != vmClone.Status.EgressGatewayReady {
			changed = true
		}
	}

	// Update status.
	vmClone.Status.EgressGatewayReady = egressEnabled

	if err := r.Client.Status().Update(ctx, vmClone); err != nil {
		if errors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	if changed {
		logger.Info("Egress gateway state changed", "enabled", egressEnabled, "anyVMRunning", anyVMRunning)
	}

	// Poll periodically to detect VM power changes (VMI creation/deletion).
	if changed {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *VirtualMachineCloneReconciler) handleDeletion(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(vmClone, cloneFinalizerName) {
		return ctrl.Result{}, nil
	}

	cloneNS := vmClone.Status.CloneNamespace
	cloneID := vmClone.Status.CloneID

	// Delete namespaced resources by clone label FIRST, so KubeOVN can release
	// their IPs before the subnets they sit on are torn down below.
	if cloneNS != "" {
		listOpts := []client.ListOption{
			client.InNamespace(cloneNS),
			client.MatchingLabels{"ruddervirt.io/clone": cloneID},
		}

		for _, gvk := range []schema.GroupVersionKind{
			{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"},
			{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"},
			{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
			{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"},
			{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"},
		} {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(gvk)
			if err := r.Client.List(ctx, list, listOpts...); err != nil {
				logger.Error(err, "Failed to list resources for cleanup", "gvk", gvk)
				continue
			}
			for i := range list.Items {
				// Never delete shared base snapshots during per-clone teardown.
				// They are reused by every clone of the same template and are
				// garbage-collected with the build instead. Deleting one here
				// strands every other clone's PVCs in Pending ("snapshot is
				// currently being deleted"). New base snapshots no longer carry a
				// clone label, but legacy ones still do and would match the list
				// above, so this guard catches them by the base-snapshot label.
				if _, isBaseSnapshot := list.Items[i].GetLabels()["ruddervirt.io/base-snapshot"]; isBaseSnapshot {
					continue
				}
				if err := r.Client.Delete(ctx, &list.Items[i]); err != nil && !errors.IsNotFound(err) {
					logger.Error(err, "Failed to delete resource", "gvk", gvk, "name", list.Items[i].GetName())
				}
			}
		}
	}

	// Delete cluster-scoped network resources (egress gateways → subnets →
	// VPCs), discovered by the clone-id label unioned with status so a lost
	// status can't orphan them. Gate finalizer removal on full teardown — see
	// TeardownNetwork. Mirrors the build controller's deletion path.
	if cloneID != "" {
		var statusVPCs, statusSubnets []string
		if vmClone.Status.Network != nil {
			statusVPCs = vmClone.Status.Network.VPCsCreated
			statusSubnets = vmClone.Status.Network.SubnetsCreated
		}
		sel := map[string]string{"ruddervirt.io/clone": cloneID}
		done, err := network.TeardownNetwork(ctx, r.Client, cloneID, cloneNS, sel, statusVPCs, statusSubnets)
		if err != nil {
			logger.Error(err, "Network teardown failed, requeueing")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if !done {
			logger.Info("Network resources still present, requeueing")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Drop this clone's subnet CIDRs from the ovn40subnets ipset — but only
		// the ones no other build/clone still declares (clones of the same
		// template all share CIDRs, and the ipset holds one entry per CIDR).
		// Must run after TeardownNetwork reports done so this clone's own
		// subnets no longer count as users. Best-effort, like the add path.
		if r.RESTConfig != nil {
			for _, cidr := range r.cloneSubnetCIDRs(ctx, vmClone) {
				if err := network.RemoveIPSetEntryIfUnused(ctx, r.Client, r.RESTConfig, cidr); err != nil {
					logger.Error(err, "Failed to remove subnet CIDR from ovn40subnets ipset", "cidr", cidr)
				}
			}
		}
	}

	// Delete the VirtualMachineNamespace CR.
	if cloneID := vmClone.Status.CloneID; cloneID != "" {
		vmns := &v1alpha1.VirtualMachineNamespace{}
		vmns.Name = cloneID
		vmns.Namespace = vmClone.Status.CloneNamespace
		if err := r.Client.Delete(ctx, vmns); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete VirtualMachineNamespace", "vmns", cloneID)
		}
	}

	// Re-fetch to get latest resourceVersion — cleanup above may have
	// triggered reconciles that updated the object.
	latest := &v1alpha1.VirtualMachineClone{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: vmClone.Name, Namespace: vmClone.Namespace}, latest); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetching clone for finalizer removal: %w", err)
	}
	controllerutil.RemoveFinalizer(latest, cloneFinalizerName)
	if err := r.Client.Update(ctx, latest); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// cloneSubnetCIDRs returns the CIDRs of the clone's subnets, derived the same
// way handleNetworking derives them (explicit spec first, then the template's
// topology annotation), including the same positional default for an empty
// CIDR. Used at deletion time, when the live subnets are already gone and can
// no longer be read. Best-effort: returns nil when the template has been
// deleted out from under the clone.
func (r *VirtualMachineCloneReconciler) cloneSubnetCIDRs(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) []string {
	var subnets []v1alpha1.Subnet
	if vmClone.Spec.Network != nil {
		subnets = vmClone.Spec.Network.Subnets
	} else if topo, err := r.extractTopoFromTemplate(ctx, vmClone); err == nil && topo != nil {
		for _, s := range topo.Subnets {
			subnets = append(subnets, v1alpha1.Subnet{Name: s.Name, CIDR: s.CIDR})
		}
	}
	cidrs := make([]string, 0, len(subnets))
	for i, s := range subnets {
		cidr := s.CIDR
		if cidr == "" {
			cidr = fmt.Sprintf("10.0.%d.0/24", i)
		}
		cidrs = append(cidrs, cidr)
	}
	return cidrs
}

func (r *VirtualMachineCloneReconciler) failClone(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone, reason error) (ctrl.Result, error) {
	vmClone.Status.Phase = v1alpha1.ClonePhaseFailed
	vmClone.Status.Message = reason.Error()
	now := metav1.Now()
	vmClone.Status.CompletionTime = &now

	meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.CloneConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  "Failed",
		Message: reason.Error(),
	})

	if err := r.Client.Status().Update(ctx, vmClone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// resolveAgeAnchor resolves Status.AgeAnchor from the spec's replacesCloneID
// (looked up in the cluster) or spec.ageAnchor as a fallback. Sets the
// AgeAnchorResolved condition to record the outcome.
func (r *VirtualMachineCloneReconciler) resolveAgeAnchor(ctx context.Context, vmClone *v1alpha1.VirtualMachineClone) {
	logger := log.FromContext(ctx)

	if vmClone.Spec.ReplacesCloneID == "" && vmClone.Spec.AgeAnchor == nil {
		return
	}

	if vmClone.Spec.ReplacesCloneID != "" {
		cloneList := &v1alpha1.VirtualMachineCloneList{}
		if err := r.Client.List(ctx, cloneList); err != nil {
			logger.Error(err, "Listing VirtualMachineClones for replacesCloneID lookup", "replacesCloneID", vmClone.Spec.ReplacesCloneID)
		} else {
			for i := range cloneList.Items {
				prev := &cloneList.Items[i]
				if prev.UID == vmClone.UID {
					continue
				}
				if prev.Status.CloneID != vmClone.Spec.ReplacesCloneID {
					continue
				}
				anchor := prev.Status.AgeAnchor
				if anchor == nil {
					t := prev.CreationTimestamp
					anchor = &t
				}
				vmClone.Status.AgeAnchor = anchor
				meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
					Type:    v1alpha1.CloneConditionAgeAnchorResolved,
					Status:  metav1.ConditionTrue,
					Reason:  "Inherited",
					Message: fmt.Sprintf("inherited age anchor %s from clone %s (cloneID %s)", anchor.UTC().Format(time.RFC3339), prev.Name, prev.Status.CloneID),
				})
				return
			}
		}
	}

	if vmClone.Spec.AgeAnchor != nil {
		t := *vmClone.Spec.AgeAnchor
		vmClone.Status.AgeAnchor = &t
		meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
			Type:    v1alpha1.CloneConditionAgeAnchorResolved,
			Status:  metav1.ConditionTrue,
			Reason:  "Explicit",
			Message: fmt.Sprintf("using spec.ageAnchor %s", t.UTC().Format(time.RFC3339)),
		})
		return
	}

	logger.Info("predecessor clone not found, no spec.ageAnchor set; proceeding without inherited age",
		"replacesCloneID", vmClone.Spec.ReplacesCloneID)
	meta.SetStatusCondition(&vmClone.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.CloneConditionAgeAnchorResolved,
		Status:  metav1.ConditionFalse,
		Reason:  "NotFound",
		Message: fmt.Sprintf("predecessor clone with cloneID %q not found and no spec.ageAnchor provided", vmClone.Spec.ReplacesCloneID),
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualMachineCloneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.snapshotManager = clone.SnapshotManager{Client: r.Client}
	r.volumeManager = clone.VolumeManager{Client: r.Client}

	// Watch VMIs so the reconciler reacts immediately when VMs start/stop,
	// allowing automatic egress gateway scaling.
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance",
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VirtualMachineClone{}).
		Watches(vmi, handler.EnqueueRequestsFromMapFunc(r.vmiToClone)).
		Named("virtualmachineclone").
		Complete(r)
}

// vmiToClone maps a VMI event to the owning VirtualMachineClone reconcile request.
func (r *VirtualMachineCloneReconciler) vmiToClone(ctx context.Context, obj client.Object) []ctrl.Request {
	labels := obj.GetLabels()
	cloneID := labels["ruddervirt.io/clone"]
	if cloneID == "" {
		return nil
	}

	// Find the clone CR that owns this cloneID.
	cloneList := &v1alpha1.VirtualMachineCloneList{}
	if err := r.Client.List(ctx, cloneList); err != nil {
		return nil
	}
	for _, c := range cloneList.Items {
		if c.Status.CloneID == cloneID {
			return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(&c)}}
		}
	}
	return nil
}
