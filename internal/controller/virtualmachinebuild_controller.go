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
	"net"
	"os"
	"strings"
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
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	"github.com/ruddervirt/aileron/internal/namespace"
	"github.com/ruddervirt/aileron/internal/network"
)

const (
	requeueInterval = 15 * time.Second
	finalizerName   = "ruddervirt.io/cleanup"

	// disksNormalizedAnnotation marks that disk normalization (inheriting the
	// boot disk for buildRef sources) has already run, so it is not applied
	// twice across the requeues of the initial-setup phase.
	disksNormalizedAnnotation = "ruddervirt.io/disks-normalized"

	// ServiceAccountName is the service account used by the controller.
	ServiceAccountName = "aileron-controller-manager"
)

// OperatorNamespace returns the namespace where the controller runs.
// Set via OPERATOR_NAMESPACE env var (injected by Helm from .Release.Namespace).
func OperatorNamespace() string {
	if ns := os.Getenv("OPERATOR_NAMESPACE"); ns != "" {
		return ns
	}
	return "ruddervirt-system"
}

// VirtualMachineBuildReconciler reconciles a VirtualMachineBuild object.
type VirtualMachineBuildReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	RESTConfig  *rest.Config
	BuildLimits *build.BuildLimits
}

// +kubebuilder:rbac:groups=ruddervirt.io,resources=virtualmachinebuilds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ruddervirt.io,resources=virtualmachinebuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ruddervirt.io,resources=virtualmachinebuilds/finalizers,verbs=update
// +kubebuilder:rbac:groups=ruddervirt.io,resources=virtualmachinenamespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ruddervirt.io,resources=virtualmachinenamespaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=kubevirts,verbs=get;list;patch;update
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/portforward,verbs=get
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/vnc,verbs=get
// +kubebuilder:rbac:groups=kubeovn.io,resources=vpcs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeovn.io,resources=subnets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeovn.io,resources=vpc-egress-gateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotcontents,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch

// reconcileTimeout bounds the entire reconcile cycle so a stuck SSH or
// SPDY connection cannot hang the controller goroutine indefinitely.
const reconcileTimeout = 2 * time.Minute

//nolint:gocyclo // reconciliation state machine with many phases
func (r *VirtualMachineBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	logger := logf.FromContext(ctx)

	vmBuild := &v1alpha1.VirtualMachineBuild{}
	if err := r.Get(ctx, req.NamespacedName, vmBuild); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion via finalizer.
	if !vmBuild.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, vmBuild)
	}

	logger.Info("Reconcile START", "phase", vmBuild.Status.Phase, "message", vmBuild.Status.Message,
		"resourceVersion", vmBuild.ResourceVersion)

	// Add finalizer if not present.
	if !controllerutil.ContainsFinalizer(vmBuild, finalizerName) {
		finalizerPatch := client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"finalizers":["`+finalizerName+`"]}}`))
		if err := r.Patch(ctx, vmBuild, finalizerPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize: generate BuildID, normalize spec, set initial phase.
	if vmBuild.Status.Phase == "" {
		buildID := namespace.GenerateBuildID()

		// Step 0: Enforce build limits before any spec normalization.
		// Hard limits (VM count, disk count) fail the build immediately.
		// Soft limits (CPU, memory, disk size) clamp and persist.
		if r.BuildLimits != nil {
			limChanged, limMessages, limErr := r.BuildLimits.EnforceLimits(&vmBuild.Spec)
			if limErr != nil {
				// Hard fail — set minimal status so the failure is visible.
				now := metav1.Now()
				vmBuild.Status.Phase = v1alpha1.BuildPhaseFailed
				vmBuild.Status.StartTime = &metav1.Time{Time: now.Time}
				vmBuild.Status.CompletionTime = &now
				vmBuild.Status.BuildID = buildID
				vmBuild.Status.BuildNamespace = OperatorNamespace()
				vmBuild.Status.VirtualMachineNamespace = buildID
				vmBuild.Status.Message = limErr.Error()
				r.setPhaseCondition(vmBuild, v1alpha1.BuildPhaseFailed)
				r.initVMStatuses(vmBuild)
				if statusErr := r.Status().Update(ctx, vmBuild); statusErr != nil {
					return ctrl.Result{}, fmt.Errorf("setting failed status for build limit: %w", statusErr)
				}
				return ctrl.Result{}, nil
			}
			if limChanged {
				// Persist clamped spec. The Update triggers a requeue so the
				// next reconcile sees the adjusted values.
				for _, msg := range limMessages {
					logger.Info("Build limit enforced", "detail", msg)
				}
				// Store messages as annotation so the status init pass can
				// surface them.
				annotations := vmBuild.GetAnnotations()
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations["ruddervirt.io/limit-messages"] = strings.Join(limMessages, "; ")
				vmBuild.SetAnnotations(annotations)
				if err := r.Update(ctx, vmBuild); err != nil {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{Requeue: true}, nil
			}
		}

		// Step 1: Normalize the spec FIRST (before any status writes).
		// This avoids the race where a spec update triggers a concurrent
		// reconcile that overwrites status before buildID is persisted.
		ph := &pendingHandler{client: r.Client}
		specChanged := ph.normalizeNetwork(vmBuild)

		// Pin a stable MAC on every NIC missing one so the existing
		// template-VM topology annotation carries the assignment to
		// clones — without this, libvirt picks a fresh random MAC at
		// boot and the cloned guest sees "new" hardware on every NIC.
		if build.MaterializeMACs(vmBuild) {
			specChanged = true
		}

		// Default EFIFirmware for VMs that need it. A caller may omit this
		// field even when the YAML defines it (zero-value struct).
		// Also normalize disks for buildRef parents: the boot disk is inherited
		// from the parent and the child's listed disks become additional data
		// disks. Gated by an annotation so the inherited boot disk isn't
		// prepended again on later requeues of the initial-setup phase.
		disksNormalized := vmBuild.Annotations[disksNormalizedAnnotation] == "true"
		for i := range vmBuild.Spec.VMs {
			if vmBuild.Spec.VMs[i].EFIFirmware == nil {
				vmBuild.Spec.VMs[i].EFIFirmware = &v1alpha1.EFIFirmware{}
				specChanged = true
			}
			if !disksNormalized {
				if changed, err := r.normalizeBuildRefDisks(ctx, &vmBuild.Spec.VMs[i]); err != nil {
					logger.Error(err, "Failed to normalize disks from parent build", "vm", vmBuild.Spec.VMs[i].Name)
				} else if changed {
					specChanged = true
				}
			}
		}
		if !disksNormalized {
			if vmBuild.Annotations == nil {
				vmBuild.Annotations = map[string]string{}
			}
			vmBuild.Annotations[disksNormalizedAnnotation] = "true"
			specChanged = true
		}

		if specChanged {
			if err := r.Update(ctx, vmBuild); err != nil {
				return ctrl.Result{Requeue: true}, nil
			}
			// Spec update replaces in-memory object. Requeue to re-read.
			return ctrl.Result{Requeue: true}, nil
		}

		// Step 2: Write status (buildID, phase, etc.) AFTER spec is stable.
		vmBuild.Status.Phase = v1alpha1.BuildPhasePending
		vmBuild.Status.StartTime = &metav1.Time{Time: time.Now()}
		vmBuild.Status.Network = nil
		vmBuild.Status.BuildID = buildID
		vmBuild.Status.BuildNamespace = OperatorNamespace()
		vmBuild.Status.VirtualMachineNamespace = buildID
		vmBuild.Status.Message = vmBuild.Annotations["ruddervirt.io/limit-messages"] // may be empty
		vmBuild.Status.CompletionTime = nil
		vmBuild.Status.Conditions = nil
		r.initVMStatuses(vmBuild)
		if err := r.Status().Update(ctx, vmBuild); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting initial status: %w", err)
		}

		return ctrl.Result{Requeue: true}, nil
	}

	// Guard: ensure buildID is persisted. If a previous reconcile's status
	// write was lost due to a conflict, the buildID might be empty.
	// Look for an existing VirtualMachineNamespace owned by this build to recover.
	if vmBuild.Status.BuildID == "" && vmBuild.Status.Phase != "" {
		vmnsList := &v1alpha1.VirtualMachineNamespaceList{}
		if err := r.List(ctx, vmnsList,
			client.InNamespace(OperatorNamespace()),
			client.MatchingLabels{build.LabelBuild: vmBuild.Name},
		); err == nil && len(vmnsList.Items) > 0 {
			// Recover buildID from existing VMNS.
			recovered := vmnsList.Items[0].Name
			logger.Info("Recovered buildID from existing VirtualMachineNamespace", "buildID", recovered)
			vmBuild.Status.BuildID = recovered
			vmBuild.Status.BuildNamespace = OperatorNamespace()
			vmBuild.Status.VirtualMachineNamespace = recovered
		} else {
			// No VMNS found — generate fresh.
			logger.Info("BuildID missing, generating new")
			vmBuild.Status.BuildID = namespace.GenerateBuildID()
			vmBuild.Status.BuildNamespace = OperatorNamespace()
			vmBuild.Status.VirtualMachineNamespace = vmBuild.Status.BuildID
		}
		if err := r.Status().Update(ctx, vmBuild); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if build.IsTerminal(vmBuild.Status.Phase) {
		// Best-effort cleanup of expired cached ISOs.
		isoImporter := &build.ISOImporter{Client: r.Client, OperatorNS: OperatorNamespace()}
		ttl := 24 * time.Hour
		if vmBuild.Spec.ISOCacheTTL != nil {
			ttl = vmBuild.Spec.ISOCacheTTL.Duration
		}
		_ = isoImporter.CleanupExpiredISOs(ctx, OperatorNamespace(), ttl)

		if vmBuild.Status.Phase == v1alpha1.BuildPhaseFailed {
			if build.DebugMode() {
				logger.Info("Skipping failure cleanup (DEBUG=true); resources retained for inspection")
			} else if wait := failureCleanupDelay(vmBuild); wait > 0 {
				logger.Info("Deferring failure cleanup for retention window", "wait", wait)
				return ctrl.Result{RequeueAfter: wait}, nil
			} else if remaining := r.cleanupBuildResources(ctx, vmBuild); remaining > 0 {
				logger.Info("Build resources still being deleted, requeueing", "remaining", remaining)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	// Timeout check — but don't fail a build whose VMs have all already
	// succeeded. The per-VM state machine may not have had a chance to
	// advance the overall build phase yet (e.g. scheduling delays ate the
	// budget, but the actual work finished in time).
	if vmBuild.Spec.Timeout != nil && vmBuild.Status.StartTime != nil {
		if time.Since(vmBuild.Status.StartTime.Time) > vmBuild.Spec.Timeout.Duration {
			if !allVMsSucceeded(vmBuild) {
				logger.Info("Build timed out")
				return r.failBuild(ctx, vmBuild, "build timed out")
			}
			logger.Info("Build timeout reached but all VMs succeeded, allowing completion")
		}
	}

	// Ensure SSH keypair exists for this build.
	sshKeyPair, err := build.EnsureSSHKeySecret(ctx, r.Client, vmBuild)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring SSH key: %w", err)
	}

	// Build the overall state machine.
	sm := build.NewStateMachine(map[v1alpha1.BuildPhase]build.PhaseHandler{
		v1alpha1.BuildPhasePending:              &pendingHandler{client: r.Client},
		v1alpha1.BuildPhaseNetworking:           &build.NetworkSetup{Client: r.Client, RESTConfig: r.RESTConfig},
		v1alpha1.BuildPhaseBuilding:             &buildingHandler{client: r.Client, restConfig: r.RESTConfig, sshKeyPair: sshKeyPair},
		v1alpha1.BuildPhaseCapturingDisks:       &capturingHandler{client: r.Client},
		v1alpha1.BuildPhaseExporting:            &exportingHandler{client: r.Client},
		v1alpha1.BuildPhaseTemplateProvisioning: &build.TemplateProvisioner{Client: r.Client},
	})

	// Deep-snapshot VM statuses before the handler runs so we can detect changes.
	// A shallow copy shares the ProvisionerResults slice, so mutations by
	// the handler would be invisible to the change detector.
	oldVMStatuses := make([]v1alpha1.VMBuildStatus, len(vmBuild.Status.VMStatuses))
	for i, vs := range vmBuild.Status.VMStatuses {
		oldVMStatuses[i] = vs
		oldVMStatuses[i].ProvisionerResults = make([]v1alpha1.ProvisionerResult, len(vs.ProvisionerResults))
		copy(oldVMStatuses[i].ProvisionerResults, vs.ProvisionerResults)
	}

	nextPhase, err := sm.Step(ctx, vmBuild)
	if err != nil {
		logger.Error(err, "Phase handler error", "phase", vmBuild.Status.Phase)
		if nextPhase == v1alpha1.BuildPhaseFailed {
			return r.failBuild(ctx, vmBuild, err.Error())
		}
		// Don't persist status on non-fatal errors — the next reconcile will pick up
		// fresh state. Persisting here risks overwriting status changes made by a
		// concurrent reconcile (e.g., phase regression from Building to Pending).
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	phaseChanged := nextPhase != vmBuild.Status.Phase
	if phaseChanged {
		logger.Info("Phase transition", "from", vmBuild.Status.Phase, "to", nextPhase)
		vmBuild.Status.Phase = nextPhase
		r.setPhaseCondition(vmBuild, nextPhase)

		if build.IsTerminal(nextPhase) {
			now := metav1.Now()
			vmBuild.Status.CompletionTime = &now
		}
	}

	// Check if status actually changed.
	statusChanged := phaseChanged
	if !statusChanged {
		if len(vmBuild.Status.VMStatuses) != len(oldVMStatuses) {
			statusChanged = true
		} else {
			for i, vs := range vmBuild.Status.VMStatuses {
				old := oldVMStatuses[i]
				if old.Phase != vs.Phase || old.OutputDataVolume != vs.OutputDataVolume ||
					old.Message != vs.Message || len(old.ProvisionerResults) != len(vs.ProvisionerResults) {
					statusChanged = true
					break
				}
				for j := range vs.ProvisionerResults {
					if j >= len(old.ProvisionerResults) || old.ProvisionerResults[j].Status != vs.ProvisionerResults[j].Status {
						statusChanged = true
						break
					}
				}
				if statusChanged {
					break
				}
			}
		}
	}

	if statusChanged {
		logger.Info("Persisting status changes",
			"phase", vmBuild.Status.Phase,
			"vmPhases", fmt.Sprintf("%v", func() []string {
				ps := make([]string, 0, len(vmBuild.Status.VMStatuses))
				for _, vs := range vmBuild.Status.VMStatuses {
					ps = append(ps, fmt.Sprintf("%s=%s(%s)", vs.Name, vs.Phase, vs.Message))
				}
				return ps
			}()))
		if err := r.Status().Update(ctx, vmBuild); err != nil {
			if errors.IsConflict(err) {
				logger.Info("Status update conflict, attempting recovery")
				return r.recoverConflict(), nil
			}
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("No status changes, requeueing", "phase", vmBuild.Status.Phase)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// handleDeletion runs cleanup when the build is being deleted.
// Deletion order matters for KubeOVN: subnets must be deleted before the
// namespace (so KubeOVN can release IPs while pods still exist), and subnets
// must be fully gone before VPCs (KubeOVN dependency).
//
//nolint:gocyclo // ordered cleanup with KubeOVN dependency chain
func (r *VirtualMachineBuildReconciler) handleDeletion(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(vmBuild, finalizerName) {
		return ctrl.Result{}, nil
	}

	// Recover buildID if it wasn't persisted to status.
	if vmBuild.Status.BuildID == "" {
		vmnsList := &v1alpha1.VirtualMachineNamespaceList{}
		if err := r.List(ctx, vmnsList,
			client.InNamespace(OperatorNamespace()),
			client.MatchingLabels{build.LabelBuild: vmBuild.Name},
		); err == nil && len(vmnsList.Items) > 0 {
			vmBuild.Status.BuildID = vmnsList.Items[0].Name
			vmBuild.Status.BuildNamespace = OperatorNamespace()
			vmBuild.Status.VirtualMachineNamespace = vmnsList.Items[0].Name
			logger.Info("Recovered buildID for deletion cleanup", "buildID", vmBuild.Status.BuildID)
		}
	}

	buildID := build.BuildID(vmBuild)
	logger.Info("Running cleanup for build deletion", "buildID", buildID)

	// Step 1: Delete all namespaced resources by build-id label.
	// VMs and pods must be deleted FIRST so KubeOVN can release their IPs
	// before we delete subnets — otherwise KubeOVN's finalizer blocks.
	r.cleanupBuildResources(ctx, vmBuild)

	// Step 2: Network cleanup — now that VMs/pods are gone, KubeOVN can
	// release IPs and remove its subnet finalizer naturally.

	// Delete every KubeOVN VPC, subnet, and egress gateway for this build,
	// discovered by the build-id label (authoritative — each is labeled at
	// creation) unioned with the names recorded in status. Driving teardown off
	// the label rather than status alone means a lost or never-written status
	// can no longer orphan the network topology, which previously left
	// egress-gateway pods wedged in Init with NoAvailableAddress long after the
	// build was gone. TeardownBuildNetwork deletes in dependency order and
	// force-removes stuck finalizers, so the build's own finalizer is only
	// removed (below) once it reports everything deleted.
	if buildID != "" {
		var statusVPCs, statusSubnets []string
		if vmBuild.Status.Network != nil {
			statusVPCs = vmBuild.Status.Network.VPCsCreated
			statusSubnets = vmBuild.Status.Network.SubnetsCreated
		}
		sel := map[string]string{build.LabelBuildID: buildID}
		done, err := network.TeardownNetwork(ctx, r.Client, buildID, build.BuildNS(vmBuild), sel, statusVPCs, statusSubnets)
		if err != nil {
			logger.Error(err, "Network teardown failed, requeueing")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if !done {
			logger.Info("Network resources still present, requeueing")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// Drop this build's subnet CIDRs from the ovn40subnets ipset — but only the
	// ones no other build/clone still declares (labs share CIDRs, and the ipset
	// holds one entry per CIDR). Must run after TeardownNetwork reports done so
	// this build's own subnets no longer count as users. Driven off the spec
	// (not status) so it runs even when status was never populated.
	if vmBuild.Spec.Network != nil {
		for _, subnet := range vmBuild.Spec.Network.Subnets {
			cidr := subnet.CIDR
			if cidr == "" {
				cidr = "10.0.0.0/24"
			}
			if err := network.RemoveIPSetEntryIfUnused(ctx, r.Client, r.RESTConfig, cidr); err != nil {
				logger.Error(err, "Failed to remove subnet CIDR from ovn40subnets ipset", "cidr", cidr)
			}
		}
	}

	// Step 3: Delete the VirtualMachineNamespace CR.
	if buildID != "" && vmBuild.Status.VirtualMachineNamespace != "" {
		vmns := &v1alpha1.VirtualMachineNamespace{}
		vmns.Name = vmBuild.Status.VirtualMachineNamespace
		vmns.Namespace = build.BuildNS(vmBuild)
		if err := r.Delete(ctx, vmns); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete VirtualMachineNamespace")
		}
	}

	controllerutil.RemoveFinalizer(vmBuild, finalizerName)
	if err := r.Update(ctx, vmBuild); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("Cleanup complete")
	return ctrl.Result{}, nil
}

// normalizeBuildRefDisks adjusts the disk list for a VM whose source is another
// build (source.buildRef). The boot disk is always inherited from the parent
// build's boot disk so its bus matches what the OS was installed on — e.g. a
// Windows image built on SCSI won't BSOD with INACCESSIBLE_BOOT_DEVICE when
// booted on VirtIO. Every disk the child specifies is treated as an ADDITIONAL
// data disk placed after the inherited boot disk. For non-buildRef sources the
// disk list is left unchanged (the VM's own first disk is the boot disk).
//
// This mutates vmSpec.Disks and must run exactly once; the caller gates it with
// disksNormalizedAnnotation. Returns (changed, error).
func (r *VirtualMachineBuildReconciler) normalizeBuildRefDisks(ctx context.Context, vmSpec *v1alpha1.BuildVM) (bool, error) {
	if vmSpec.Source.BuildRef == nil {
		return false, nil // boot disk comes from the VM's own first disk
	}

	ref := vmSpec.Source.BuildRef
	refNS := ref.Namespace
	if refNS == "" {
		refNS = "ruddervirt-system" // TODO: use build.Namespace once available
	}

	parent := &v1alpha1.VirtualMachineBuild{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: refNS}, parent); err != nil {
		return false, fmt.Errorf("looking up parent build %s: %w", ref.Name, err)
	}

	// Find the parent VM spec.
	parentVMName := ref.VMName
	if parentVMName == "" && len(parent.Spec.VMs) == 1 {
		parentVMName = parent.Spec.VMs[0].Name
	}

	var parentVM *v1alpha1.BuildVM
	for i := range parent.Spec.VMs {
		if parent.Spec.VMs[i].Name == parentVMName {
			parentVM = &parent.Spec.VMs[i]
			break
		}
	}
	if parentVM == nil {
		return false, fmt.Errorf("parent build %s has no VM %q", ref.Name, parentVMName)
	}

	bootDisk := build.BootDisk(parentVM)

	// The inherited boot disk and the child's data disks must have distinct
	// names, otherwise KubeVirt rejects the VM for duplicate volume names.
	for _, d := range vmSpec.Disks {
		if d.Name == bootDisk.Name {
			return false, fmt.Errorf(
				"VM %s: additional disk %q collides with inherited boot disk name; rename it",
				vmSpec.Name, d.Name)
		}
	}

	vmSpec.Disks = append([]v1alpha1.BuildDisk{bootDisk}, vmSpec.Disks...)
	return true, nil
}

// allVMsSucceeded returns true if every VM in the build has reached the
// Succeeded phase. Used by the timeout check to avoid failing a build that
// has actually completed but whose overall phase hasn't been advanced yet.
func allVMsSucceeded(vmBuild *v1alpha1.VirtualMachineBuild) bool {
	if len(vmBuild.Status.VMStatuses) == 0 {
		return false
	}
	for _, vs := range vmBuild.Status.VMStatuses {
		if vs.Phase != v1alpha1.VMPhaseSucceeded {
			return false
		}
	}
	return true
}

// failureCleanupDelay returns how long the controller should wait before
// tearing down a Failed build's resources, so coordinator logs are inspectable
// post-mortem. Returns 0 (cleanup now) when retention is disabled, the build
// has no CompletionTime, or the retention window has already elapsed.
func failureCleanupDelay(vmBuild *v1alpha1.VirtualMachineBuild) time.Duration {
	retention := build.FailureRetention()
	if retention <= 0 || vmBuild.Status.CompletionTime == nil {
		return 0
	}
	remaining := retention - time.Since(vmBuild.Status.CompletionTime.Time)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func (r *VirtualMachineBuildReconciler) initVMStatuses(vmBuild *v1alpha1.VirtualMachineBuild) {
	vmBuild.Status.VMStatuses = make([]v1alpha1.VMBuildStatus, len(vmBuild.Spec.VMs))
	for i, vm := range vmBuild.Spec.VMs {
		vmBuild.Status.VMStatuses[i] = v1alpha1.VMBuildStatus{
			Name:  vm.Name,
			Phase: v1alpha1.VMPhasePending,
		}
	}
	// Reset network status to prevent stale data from previous builds.
	vmBuild.Status.Network = nil
}

//nolint:gocyclo // stale-phase guard logic requires many comparisons
func (r *VirtualMachineBuildReconciler) failBuild(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild, message string) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("FAILBUILD CALLED", "message", message, "currentPhase", vmBuild.Status.Phase)

	// Re-fetch the latest version to avoid stale resourceVersion conflicts.
	key := types.NamespacedName{Name: vmBuild.Name, Namespace: vmBuild.Namespace}
	latest := &v1alpha1.VirtualMachineBuild{}
	if err := r.Get(ctx, key, latest); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetching for failBuild: %w", err)
	}

	// Guard: if the latest object has already progressed past this reconcile's
	// view, another reconcile already advanced the state. Don't regress to Failed.
	if latest.Status.Phase != vmBuild.Status.Phase {
		logger.Info("failBuild: latest phase differs from reconcile phase, skipping",
			"latestPhase", latest.Status.Phase, "reconcilePhase", vmBuild.Status.Phase)
		return ctrl.Result{Requeue: true}, nil
	}
	// Also check VM-level phases: if any VM has progressed forward beyond
	// what this reconcile saw, the handler's failure is stale. But if the
	// stale phase is Failed, that's the handler signaling an error — allow it.
	for i := range latest.Status.VMStatuses {
		if i >= len(vmBuild.Status.VMStatuses) {
			break
		}
		latestVM := latest.Status.VMStatuses[i]
		staleVM := vmBuild.Status.VMStatuses[i]
		if latestVM.Phase != staleVM.Phase && staleVM.Phase != v1alpha1.VMPhaseFailed {
			logger.Info("failBuild: VM phase diverged, skipping",
				"vm", latestVM.Name, "latestVMPhase", latestVM.Phase, "staleVMPhase", staleVM.Phase)
			return ctrl.Result{Requeue: true}, nil
		}
	}

	latest.Status.Phase = v1alpha1.BuildPhaseFailed
	// Truncate message to fit Kubernetes condition limits (32KB max).
	// Keep the tail — the error reason is usually at the end.
	if len(message) > 8000 {
		message = "(truncated) ..." + message[len(message)-8000:]
	}
	latest.Status.Message = message
	now := metav1.Now()
	latest.Status.CompletionTime = &now
	meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type: "Failed", Status: metav1.ConditionTrue,
		Reason: "BuildFailed", Message: message, LastTransitionTime: now,
	})
	if err := r.Status().Update(ctx, latest); err != nil {
		if errors.IsConflict(err) {
			logger.Info("failBuild: status update conflict, will retry")
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "FAILBUILD STATUS UPDATE FAILED")
		return ctrl.Result{}, fmt.Errorf("updating failed status: %w", err)
	}
	logger.Info("FAILBUILD STATUS UPDATE SUCCEEDED")
	return ctrl.Result{}, nil
}

// cleanupBuildResources deletes all namespaced resources associated with a
// build by label. Called both on failure (to free cluster resources) and
// during CR deletion (as the first step of the finalizer). Network
// infrastructure (VPCs, subnets) is NOT deleted here — that requires the
// ordered teardown in handleDeletion.
func (r *VirtualMachineBuildReconciler) cleanupBuildResources(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild) int {
	logger := logf.FromContext(ctx)
	buildID := build.BuildID(vmBuild)
	if buildID == "" {
		return 0
	}
	buildNS := build.BuildNS(vmBuild)

	logger.Info("Deleting build resources by label", "buildID", buildID, "namespace", buildNS)

	listOpts := []client.ListOption{
		client.InNamespace(buildNS),
		client.MatchingLabels{build.LabelBuildID: buildID},
	}

	remaining := 0
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"},
		{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"},
		{Group: "", Version: "v1", Kind: "Pod"},
		{Group: "batch", Version: "v1", Kind: "Job"},
		{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"},
		// Base snapshots created by the clone controller inherit this build's
		// build-id label, so they are cleaned up here with the build rather than
		// leaking (their lifecycle is the build's, not any single clone's).
		{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"},
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
		{Group: "", Version: "v1", Kind: "Secret"},
		{Group: "", Version: "v1", Kind: "ConfigMap"},
		{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"},
	} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		if err := r.List(ctx, list, listOpts...); err != nil {
			logger.Error(err, "Failed to list resources for cleanup", "gvk", gvk)
			continue
		}
		for i := range list.Items {
			if err := r.Delete(ctx, &list.Items[i]); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "Failed to delete resource", "gvk", gvk, "name", list.Items[i].GetName())
			}
			remaining++
		}
	}

	if vmBuild.Status.Network != nil {
		for _, vpcName := range vmBuild.Status.Network.VPCsCreated {
			gwName := vpcName + "-egress"
			if err := network.DeleteEgressGateway(ctx, r.Client, gwName, buildNS); err != nil {
				logger.Error(err, "Failed to delete egress gateway", "gateway", gwName)
			}
		}
	}

	return remaining
}

func (r *VirtualMachineBuildReconciler) recoverConflict() ctrl.Result {
	return ctrl.Result{Requeue: true}
}

func (r *VirtualMachineBuildReconciler) setPhaseCondition(vmBuild *v1alpha1.VirtualMachineBuild, phase v1alpha1.BuildPhase) {
	now := metav1.Now()
	var condType, reason, message string
	switch phase {
	case v1alpha1.BuildPhaseBuilding:
		condType = v1alpha1.ConditionNetworkReady
		reason = "NetworkReady"
		message = "KubeOVN network topology is ready"
	case v1alpha1.BuildPhaseCapturingDisks:
		condType = v1alpha1.ConditionAllVMsReady
		reason = "AllVMsProvisioned"
		message = "All VMs have been provisioned and shut down"
	case v1alpha1.BuildPhaseExporting, v1alpha1.BuildPhaseTemplateProvisioning:
		condType = v1alpha1.ConditionDisksCapture
		reason = "DisksCaptured"
		message = "All VM disks cloned to output DataVolumes"
	case v1alpha1.BuildPhaseSucceeded:
		condType = v1alpha1.ConditionTemplateProvisioned
		reason = "TemplateProvisioned"
		message = "Template VMs created and ephemeral resources cleaned up"
	default:
		return
	}
	meta.SetStatusCondition(&vmBuild.Status.Conditions, metav1.Condition{
		Type: condType, Status: metav1.ConditionTrue,
		Reason: reason, Message: message, LastTransitionTime: now,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualMachineBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VirtualMachineBuild{}).
		Named("virtualmachinebuild").
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: 10}).
		Complete(r)
}

// =============================================================================
// Phase Handlers
// =============================================================================

// pendingHandler validates, creates the child namespace, and transitions to Networking.
type pendingHandler struct {
	client client.Client
}

const autoVPCName = "vpc"

func (h *pendingHandler) Handle(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild) (v1alpha1.BuildPhase, error) {
	logger := logf.FromContext(ctx)

	if len(vmBuild.Spec.VMs) == 0 {
		return v1alpha1.BuildPhaseFailed, fmt.Errorf("spec.vms must contain at least one VM")
	}
	for _, vm := range vmBuild.Spec.VMs {
		src := vm.Source
		if src.URL == "" && src.SourcePVC == nil && src.ContainerDisk == "" && src.BuildRef == nil && !src.Blank {
			return v1alpha1.BuildPhaseFailed, fmt.Errorf("VM %s: no source specified (set url, sourcePvc, containerDisk, blank, or buildRef)", vm.Name)
		}
		for i, iso := range vm.ISOs {
			if iso.URL == "" {
				return v1alpha1.BuildPhaseFailed, fmt.Errorf("VM %s: iso[%d].url is required", vm.Name, i)
			}
		}
	}
	// All VMs are automatically captured — no output validation needed.

	// Create the VirtualMachineNamespace CR if it doesn't exist.
	// BuildID was already set during initialization.
	buildID := vmBuild.Status.BuildID
	vmnsName := vmBuild.Status.VirtualMachineNamespace
	if vmnsName != "" {
		vmns := &v1alpha1.VirtualMachineNamespace{}
		vmns.Name = vmnsName
		vmns.Namespace = OperatorNamespace()
		vmns.Spec = v1alpha1.VirtualMachineNamespaceSpec{
			OwnerRef: &v1alpha1.VirtualMachineNamespaceOwnerRef{
				Kind:      "VirtualMachineBuild",
				Name:      vmBuild.Name,
				Namespace: vmBuild.Namespace,
			},
		}
		vmns.Labels = map[string]string{
			build.LabelBuildID: buildID,
			build.LabelBuild:   vmBuild.Name,
		}
		if err := h.client.Create(ctx, vmns); err != nil {
			if !errors.IsAlreadyExists(err) {
				return v1alpha1.BuildPhaseFailed, fmt.Errorf("creating VirtualMachineNamespace: %w", err)
			}
		}
		logger.Info("Ensured VirtualMachineNamespace", "vmns", vmnsName, "buildID", buildID)
	}

	// Network spec was already normalized during initialization.
	if err := validateNetworkSpec(vmBuild); err != nil {
		return v1alpha1.BuildPhaseFailed, err
	}

	if vmBuild.Spec.Network != nil && (len(vmBuild.Spec.Network.VPCs) > 0 || len(vmBuild.Spec.Network.Subnets) > 0) {
		return v1alpha1.BuildPhaseNetworking, nil
	}
	return v1alpha1.BuildPhaseBuilding, nil
}

// validateNetworkSpec checks the build's network references and per-subnet
// constraints (VPC refs, unmanaged-subnet rules, VM/override NIC assignments).
// Returns a non-nil error describing the first problem, which the caller
// surfaces by failing the build.
func validateNetworkSpec(vmBuild *v1alpha1.VirtualMachineBuild) error {
	if vmBuild.Spec.Network == nil {
		return nil
	}
	vpcNames := make(map[string]bool)
	for _, vpc := range vmBuild.Spec.Network.VPCs {
		vpcNames[vpc.Name] = true
	}
	subnetsByName := make(map[string]*v1alpha1.Subnet, len(vmBuild.Spec.Network.Subnets))
	for i := range vmBuild.Spec.Network.Subnets {
		subnet := &vmBuild.Spec.Network.Subnets[i]
		subnetsByName[subnet.Name] = subnet
		if !vpcNames[subnet.VPC] {
			return fmt.Errorf("subnet %q references unknown VPC %q", subnet.Name, subnet.VPC)
		}
		// Keyed on the base spec flag (not the build override): the constraints
		// protect the clone-time unmanaged realization, which clones inherit
		// regardless of how the build itself was overridden.
		if subnet.Unmanaged {
			// The guest gateway owns DHCP/DNS on unmanaged segments, and the
			// CIDR must leave room to park OVN's mandatory gateway router port
			// away from the guest gateway (see SubnetSpec.ApplyUnmanaged).
			if subnet.DNS != "" {
				return fmt.Errorf("subnet %q: dns has no effect on unmanaged subnets (the guest gateway serves DHCP/DNS); remove it", subnet.Name)
			}
			if _, err := network.UnmanagedGateway(subnet.CIDR); err != nil {
				return fmt.Errorf("subnet %q: %w", subnet.Name, err)
			}
		}
	}
	for _, vm := range vmBuild.Spec.VMs {
		if err := validateVMNICs(vm.Name, "", vm.NICs, subnetsByName); err != nil {
			return err
		}
	}
	if vmBuild.Spec.BuildOverrides != nil {
		for _, o := range vmBuild.Spec.BuildOverrides.VMs {
			if len(o.NICs) == 0 {
				continue
			}
			if err := validateVMNICs(o.Name, "override ", o.NICs, subnetsByName); err != nil {
				return err
			}
		}
		for _, o := range vmBuild.Spec.BuildOverrides.Subnets {
			if _, ok := subnetsByName[o.Name]; !ok {
				return fmt.Errorf("buildOverrides subnet %q does not match any spec.network.subnets[]", o.Name)
			}
		}
	}
	return nil
}

// validateVMNICs checks a NIC list against the build's defined subnets. scope
// is a free-text label ("" for base spec, "override " for buildOverrides) that
// gets woven into error messages so the operator knows which list to fix.
func validateVMNICs(vmName, scope string, nics []v1alpha1.VMNIC, subnetsByName map[string]*v1alpha1.Subnet) error {
	slotOwners := make(map[int]string)
	for _, nic := range nics {
		subnet, ok := subnetsByName[nic.Subnet]
		if !ok {
			return fmt.Errorf("VM %s %sNIC %s references unknown subnet %q", vmName, scope, nic.Name, nic.Subnet)
		}
		if nic.Slot != 0 {
			if prev, dup := slotOwners[nic.Slot]; dup {
				return fmt.Errorf("VM %s %sNICs %s and %s both claim slot %d", vmName, scope, prev, nic.Name, nic.Slot)
			}
			slotOwners[nic.Slot] = nic.Name
		}
		if nic.IP == "" {
			continue
		}
		ip := net.ParseIP(nic.IP)
		if ip == nil {
			return fmt.Errorf("VM %s %sNIC %s has invalid IP %q", vmName, scope, nic.Name, nic.IP)
		}
		_, cidr, err := net.ParseCIDR(subnet.CIDR)
		if err != nil {
			return fmt.Errorf("subnet %q has invalid CIDR %q: %w", subnet.Name, subnet.CIDR, err)
		}
		if !cidr.Contains(ip) {
			return fmt.Errorf("VM %s %sNIC %s IP %s is not within subnet %q CIDR %s", vmName, scope, nic.Name, nic.IP, subnet.Name, subnet.CIDR)
		}
	}
	return nil
}

// normalizeNetwork fills in defaults for the network spec.
// All VMs MUST have at least one NIC (no masquerade). If a VM has no NICs,
// inject a default subnet and NIC.
func (h *pendingHandler) normalizeNetwork(vmBuild *v1alpha1.VirtualMachineBuild) bool {
	logger := logf.Log.WithName("normalize-network")
	changed := false

	// Ensure every VM has at least one NIC — inject default subnet if needed.
	needsDefaultSubnet := false
	for _, vm := range vmBuild.Spec.VMs {
		if len(vm.NICs) == 0 {
			needsDefaultSubnet = true
			break
		}
	}

	if needsDefaultSubnet {
		logger.Info("Injecting default network for VMs without NICs")
		if vmBuild.Spec.Network == nil {
			vmBuild.Spec.Network = &v1alpha1.Network{}
		}
		// Check if "default" subnet already exists.
		hasDefault := false
		for _, s := range vmBuild.Spec.Network.Subnets {
			if s.Name == "default" {
				hasDefault = true
				break
			}
		}
		if !hasDefault {
			vmBuild.Spec.Network.Subnets = append(vmBuild.Spec.Network.Subnets, v1alpha1.Subnet{
				Name: "default",
				CIDR: "10.0.0.0/24",
			})
			changed = true
		}
		for i := range vmBuild.Spec.VMs {
			if len(vmBuild.Spec.VMs[i].NICs) == 0 {
				vmBuild.Spec.VMs[i].NICs = append(vmBuild.Spec.VMs[i].NICs, v1alpha1.VMNIC{
					Name:   "eth0",
					Subnet: "default",
				})
				changed = true
			}
		}
	}

	hasSubnets := vmBuild.Spec.Network != nil && len(vmBuild.Spec.Network.Subnets) > 0
	if !hasSubnets {
		return changed
	}

	// Auto-create the build VPC if any subnet lacks a vpc reference.
	needsAutoVPC := false
	for _, subnet := range vmBuild.Spec.Network.Subnets {
		if subnet.VPC == "" {
			needsAutoVPC = true
			break
		}
	}
	if needsAutoVPC {
		hasAutoVPC := false
		for _, vpc := range vmBuild.Spec.Network.VPCs {
			if vpc.Name == autoVPCName {
				hasAutoVPC = true
				break
			}
		}
		if !hasAutoVPC {
			vmBuild.Spec.Network.VPCs = append(vmBuild.Spec.Network.VPCs, v1alpha1.VPC{
				Name:     autoVPCName,
				Internet: true,
			})
			changed = true
		}
		for i := range vmBuild.Spec.Network.Subnets {
			if vmBuild.Spec.Network.Subnets[i].VPC == "" {
				vmBuild.Spec.Network.Subnets[i].VPC = autoVPCName
				changed = true
			}
		}
	}

	return changed
}

// buildingHandler drives all VMs through their per-VM lifecycle in parallel.
type buildingHandler struct {
	client     client.Client
	restConfig *rest.Config
	sshKeyPair *build.SSHKeyPair
}

//nolint:gocyclo // per-VM state machine with many phase transitions
func (h *buildingHandler) Handle(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild) (v1alpha1.BuildPhase, error) {
	logger := logf.FromContext(ctx)

	// Materialize spec.files into a ConfigMap before creating the relay pod or VMs.
	if err := build.EnsureFilesConfigMap(ctx, h.client, vmBuild); err != nil {
		return v1alpha1.BuildPhaseBuilding, fmt.Errorf("ensuring files ConfigMap: %w", err)
	}

	// Ensure floppy ConfigMaps and disk images for VMs that need them.
	// The floppy image is written to the EFI PVC, so ensure that exists first.
	floppyReady := true
	for i := range vmBuild.Spec.VMs {
		if vmBuild.Spec.VMs[i].EFIFirmware != nil {
			if err := build.EnsureEFIFirmware(ctx, h.client, vmBuild, &vmBuild.Spec.VMs[i]); err != nil {
				return v1alpha1.BuildPhaseBuilding, fmt.Errorf("ensuring EFI firmware for VM %s: %w", vmBuild.Spec.VMs[i].Name, err)
			}
		}
		if vmBuild.Spec.VMs[i].Floppy != nil {
			if err := build.EnsureFloppyConfigMap(ctx, h.client, vmBuild, &vmBuild.Spec.VMs[i]); err != nil {
				return v1alpha1.BuildPhaseBuilding, fmt.Errorf("ensuring floppy ConfigMap for VM %s: %w", vmBuild.Spec.VMs[i].Name, err)
			}
			if err := build.EnsureFloppyImage(ctx, h.client, vmBuild, &vmBuild.Spec.VMs[i]); err != nil {
				return v1alpha1.BuildPhaseBuilding, fmt.Errorf("ensuring floppy image for VM %s: %w", vmBuild.Spec.VMs[i].Name, err)
			}
			ready, err := build.IsFloppyImageReady(ctx, h.client, vmBuild, &vmBuild.Spec.VMs[i])
			if err != nil {
				return v1alpha1.BuildPhaseFailed, fmt.Errorf("checking floppy image for VM %s: %w", vmBuild.Spec.VMs[i].Name, err)
			}
			if !ready {
				logger.Info("Waiting for floppy image to be built", "vm", vmBuild.Spec.VMs[i].Name)
				floppyReady = false
			}
		}
	}
	if !floppyReady {
		return v1alpha1.BuildPhaseBuilding, nil
	}

	// Always ensure the relay pod is running (all VMs use bridge NICs now).
	relayMgr := &build.RelayPodManager{Client: h.client}
	if err := relayMgr.EnsureRelayPod(ctx, vmBuild); err != nil {
		return v1alpha1.BuildPhaseBuilding, fmt.Errorf("ensuring relay pod: %w", err)
	}
	relayReady, err := relayMgr.IsRelayReady(ctx, vmBuild)
	if err != nil {
		return v1alpha1.BuildPhaseBuilding, fmt.Errorf("checking relay pod: %w", err)
	}
	if !relayReady {
		logger.Info("Waiting for relay pod to become ready")
		return v1alpha1.BuildPhaseBuilding, nil
	}

	sourceImporter := &build.SourceImporter{Client: h.client, OperatorNS: OperatorNamespace()}
	isoImporter := &build.ISOImporter{Client: h.client, OperatorNS: OperatorNamespace()}
	vmBooter := &build.VMBooter{Client: h.client, SSHPublicKey: h.sshKeyPair.PublicKeyAuthorized}
	vmShutdown := &build.VMShutdown{Client: h.client}

	allDone := true
	for i := range vmBuild.Spec.VMs {
		vmSpec := &vmBuild.Spec.VMs[i]
		vmStatus := &vmBuild.Status.VMStatuses[i]

		if build.IsVMTerminal(vmStatus.Phase) {
			if vmStatus.Phase == v1alpha1.VMPhaseFailed {
				return v1alpha1.BuildPhaseFailed, fmt.Errorf("VM %s failed: %s", vmSpec.Name, vmStatus.Message)
			}
			continue
		}

		allDone = false
		var nextPhase v1alpha1.VMPhase
		var err error

		switch vmStatus.Phase {
		case v1alpha1.VMPhasePending:
			nextPhase = v1alpha1.VMPhaseSourceImporting

		case v1alpha1.VMPhaseSourceImporting:
			// Pre-create the boot command Job so the image is pulled during import.
			// By the time the VM boots, the pod is already running and polling VNC.
			if len(vmSpec.BootCommand) > 0 || len(vmSpec.Provisioners) > 0 {
				coordinator := &build.CoordinatorHandler{Client: h.client, RESTConfig: h.restConfig}
				if preErr := coordinator.EnsureJob(ctx, vmBuild, vmSpec, vmStatus); preErr != nil {
					logger.Error(preErr, "Failed to pre-create coordinator Job", "vm", vmSpec.Name)
				}
			}
			nextPhase, err = sourceImporter.HandleVM(ctx, vmBuild, vmSpec, vmStatus)
			if err == nil && nextPhase == v1alpha1.VMPhaseBooting && len(vmSpec.ISOs) > 0 {
				allISOsReady, isoErr := isoImporter.HandleISOs(ctx, vmBuild, vmSpec)
				if isoErr != nil {
					nextPhase = v1alpha1.VMPhaseFailed
					err = fmt.Errorf("ISO import: %w", isoErr)
				} else if !allISOsReady {
					nextPhase = v1alpha1.VMPhaseSourceImporting
				}
			}

		case v1alpha1.VMPhaseBooting:
			nextPhase, err = vmBooter.HandleVM(ctx, vmBuild, vmSpec, vmStatus)

		case v1alpha1.VMPhaseBootCommand, v1alpha1.VMPhaseProvisioning:
			coordinator := &build.CoordinatorHandler{Client: h.client, RESTConfig: h.restConfig}
			nextPhase, err = coordinator.HandleVM(ctx, vmBuild, vmSpec, vmStatus)
			if nextPhase == v1alpha1.VMPhaseShuttingDown && !build.AllCoordinatorsComplete(ctx, h.client, vmBuild) {
				nextPhase = v1alpha1.VMPhaseProvisioning
				vmStatus.Message = "Provisioning complete, waiting for other VMs"
			}

		case v1alpha1.VMPhaseShuttingDown:
			nextPhase, err = vmShutdown.HandleVM(ctx, vmBuild, vmSpec, vmStatus)

		case v1alpha1.VMPhaseDiskCaptured:
			nextPhase = v1alpha1.VMPhaseSucceeded
		}

		if err != nil {
			logger.Error(err, "VM phase error", "vm", vmSpec.Name, "phase", vmStatus.Phase)
			if nextPhase == v1alpha1.VMPhaseFailed {
				vmStatus.Phase = v1alpha1.VMPhaseFailed
				vmStatus.Message = err.Error()
				return v1alpha1.BuildPhaseFailed, fmt.Errorf("VM %s failed: %w", vmSpec.Name, err)
			}
			continue
		}

		if nextPhase != vmStatus.Phase {
			logger.Info("VM phase transition", "vm", vmSpec.Name, "from", vmStatus.Phase, "to", nextPhase)
			vmStatus.Phase = nextPhase
		}
	}

	if allDone {
		return v1alpha1.BuildPhaseCapturingDisks, nil
	}
	return v1alpha1.BuildPhaseBuilding, nil
}

// capturingHandler drives disk capture for all VMs in parallel.
type capturingHandler struct {
	client client.Client
}

func (h *capturingHandler) Handle(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild) (v1alpha1.BuildPhase, error) {
	// Clean up the relay pod.
	relayMgr := &build.RelayPodManager{Client: h.client}
	_ = relayMgr.CleanupRelayPod(ctx, vmBuild)

	capturer := &build.DiskCapturer{Client: h.client}
	allCaptured := true

	for i := range vmBuild.Spec.VMs {
		vmSpec := &vmBuild.Spec.VMs[i]
		vmStatus := &vmBuild.Status.VMStatuses[i]

		if vmStatus.OutputDataVolume != "" {
			continue
		}

		allCaptured = false
		nextPhase, err := capturer.HandleVM(ctx, vmBuild, vmSpec, vmStatus)
		if err != nil {
			if nextPhase == v1alpha1.VMPhaseFailed {
				return v1alpha1.BuildPhaseFailed, fmt.Errorf("VM %s disk capture failed: %w", vmSpec.Name, err)
			}
			continue
		}

		if nextPhase == v1alpha1.VMPhaseSucceeded {
			vmStatus.Phase = v1alpha1.VMPhaseSucceeded
		}
	}

	if allCaptured {
		if vmBuild.Spec.S3Export != nil {
			return v1alpha1.BuildPhaseExporting, nil
		}
		return v1alpha1.BuildPhaseTemplateProvisioning, nil
	}

	return v1alpha1.BuildPhaseCapturingDisks, nil
}

// exportingHandler drives S3 export for all VMs that have it configured.
type exportingHandler struct {
	client client.Client
}

func (h *exportingHandler) Handle(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild) (v1alpha1.BuildPhase, error) {
	s3Config := vmBuild.Spec.S3Export
	exporter := &build.S3Exporter{Client: h.client}
	allExported := true

	for i := range vmBuild.Spec.VMs {
		vmSpec := &vmBuild.Spec.VMs[i]

		alreadyDone := false
		for _, exp := range vmBuild.Status.S3Exports {
			if exp.VMName == vmSpec.Name && exp.Uploaded {
				alreadyDone = true
				break
			}
		}
		if alreadyDone {
			continue
		}

		allExported = false

		nextPhase, err := exporter.HandleVMExport(ctx, vmBuild, vmSpec, s3Config)
		if err != nil {
			if nextPhase == v1alpha1.BuildPhaseFailed {
				return v1alpha1.BuildPhaseFailed, err
			}
			continue
		}
	}

	if allExported {
		return v1alpha1.BuildPhaseTemplateProvisioning, nil
	}
	return v1alpha1.BuildPhaseExporting, nil
}
