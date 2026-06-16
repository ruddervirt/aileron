package build

import (
	"context"
	"encoding/json"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/clone"
	"github.com/ruddervirt/aileron/internal/kubevirt"
	"github.com/ruddervirt/aileron/internal/network"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TemplateProvisioner handles the TemplateProvisioning phase:
// cleaning up ephemeral build resources and converting the halted build VMs
// into template VMs by patching them in-place. This preserves all domain-level
// settings (firmware, features, CPU, memory, devices) automatically — only
// instance-specific fields (volumes, networks, annotations) are stripped.
type TemplateProvisioner struct {
	Client client.Client
}

func (t *TemplateProvisioner) Handle(ctx context.Context, build *v1alpha1.VirtualMachineBuild) (v1alpha1.BuildPhase, error) {
	logger := log.FromContext(ctx)
	buildNS := BuildNS(build)

	// Step 1: Delete non-VM ephemeral resources (source DVs, ISOs, relay pod, etc.).
	t.cleanupEphemeralResources(ctx, build, buildNS)

	// Build the network topology annotation (shared across all template VMs).
	topoAnnotation := t.buildNetworkTopology(build)

	// Step 2: Convert each build VM into a template VM in-place.
	for i := range build.Spec.VMs {
		vmSpec := &build.Spec.VMs[i]
		vmName := BuildNameForBuildVM(BuildID(build), vmSpec.Name)
		pvcName := BuildNameForOutputDV(BuildID(build), vmSpec.Name)

		converted, err := t.convertToTemplate(ctx, build, buildNS, vmName, pvcName, vmSpec, topoAnnotation)
		if err != nil {
			return v1alpha1.BuildPhaseFailed, fmt.Errorf("converting build VM %s to template: %w", vmName, err)
		}
		if converted {
			logger.Info("Converted build VM to template", "vm", vmName, "namespace", buildNS)
		}
	}

	// Step 3: Set templateNamespace.
	build.Status.TemplateNamespace = buildNS

	return v1alpha1.BuildPhaseSucceeded, nil
}

// cleanupEphemeralResources deletes source DVs, ISOs, relay pod, SSH secret,
// and egress gateways from the build namespace. Build VMs are NOT deleted —
// they are converted to templates in-place by convertToTemplate.
func (t *TemplateProvisioner) cleanupEphemeralResources(ctx context.Context, build *v1alpha1.VirtualMachineBuild, buildNS string) {
	logger := log.FromContext(ctx)

	// Delete source DVs (the import DVs, not the output DVs).
	for _, vmSpec := range build.Spec.VMs {
		dvName := BuildNameForBuildVMDataVolume(BuildID(build), vmSpec.Name)
		dv := &unstructured.Unstructured{}
		dv.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume",
		})
		dv.SetName(dvName)
		dv.SetNamespace(buildNS)
		if err := t.Client.Delete(ctx, dv); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete source DV", "dv", dvName)
		}
	}

	// Delete ISO clone DVs in the build namespace.
	isoCloneList := &unstructured.UnstructuredList{}
	isoCloneList.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolumeList",
	})
	if err := t.Client.List(ctx, isoCloneList,
		client.InNamespace(buildNS),
		client.MatchingLabels{"ruddervirt.io/iso-clone": "true"},
	); err == nil {
		for i := range isoCloneList.Items {
			if err := t.Client.Delete(ctx, &isoCloneList.Items[i]); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "Failed to delete ISO clone DV", "dv", isoCloneList.Items[i].GetName())
			}
		}
	}

	// Delete relay pod.
	relayPodName := RelayPodName(BuildID(build))
	pod := &corev1.Pod{}
	pod.Name = relayPodName
	pod.Namespace = buildNS
	if err := t.Client.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "Failed to delete relay pod", "pod", relayPodName)
	}

	// Delete SSH key secret.
	secretName := SSHKeySecretName(BuildID(build))
	secret := &corev1.Secret{}
	secret.Name = secretName
	secret.Namespace = buildNS
	if err := t.Client.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "Failed to delete SSH secret", "secret", secretName)
	}

	// VPCs, subnets, and NADs are kept as part of the template (clones
	// derive network topology from them). Egress gateways are deleted
	// since template VMs are halted and don't need internet.
	if build.Status.Network != nil {
		for _, vpcName := range build.Status.Network.VPCsCreated {
			gwName := vpcName + "-egress"
			if err := network.DeleteEgressGateway(ctx, t.Client, gwName, buildNS); err != nil {
				logger.Error(err, "Failed to delete egress gateway", "gateway", gwName)
			}
		}
	}
}

// convertToTemplate patches a halted build VM in-place to become a template VM.
// The domain spec (firmware, features, CPU, memory, devices) is preserved as-is.
// Only instance-specific fields are stripped: volumes are swapped to the output
// PVC, cloud-init userData is removed (networkData kept), ISOs are dropped,
// networks get placeholder NAD references, and build-time annotations are cleared.
// Returns true if the VM was converted, false if already a template.
func (t *TemplateProvisioner) convertToTemplate(ctx context.Context, build *v1alpha1.VirtualMachineBuild, namespace, vmName, pvcName string, vmSpec *v1alpha1.BuildVM, topoAnnotation string) (bool, error) {
	// KubeVirt's VM controller writes status fields on the VM frequently
	// during the build (e.g. ready, printableStatus, observedGeneration). A
	// single get-mutate-update can lose the optimistic-concurrency race; the
	// previous build phase had succeeded but the build was reported as
	// Failed because of this. Re-fetch and re-apply on conflict.
	var converted bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm := &unstructured.Unstructured{}
		vm.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
		})
		if err := t.Client.Get(ctx, types.NamespacedName{Name: vmName, Namespace: namespace}, vm); err != nil {
			return fmt.Errorf("getting build VM: %w", err)
		}

		// Already converted — idempotent. A prior retry attempt may have
		// succeeded server-side even when the local Update returned
		// conflict, so this also short-circuits that case cleanly.
		existingLabels := vm.GetLabels()
		if existingLabels != nil && existingLabels[LabelComponent] == "template" {
			converted = false
			return nil
		}

		// --- VM-level metadata ---

		labels := map[string]string{
			LabelBuildID:        BuildID(build),
			LabelOS:             OSForShell(vmSpec.Communicator.Shell),
			clone.LabelVMName:   vmSpec.Name,
			LabelBuild:          build.Name,
			LabelBuildNamespace: build.Namespace,
			LabelComponent:      "template",
		}
		vm.SetLabels(labels)

		annotations := vm.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		if topoAnnotation != "" {
			annotations[clone.AnnotationNetworkTopology] = topoAnnotation
		}
		vm.SetAnnotations(annotations)

		// --- spec-level ---

		_ = unstructured.SetNestedField(vm.Object, "Halted", "spec", "runStrategy")
		unstructured.RemoveNestedField(vm.Object, "spec", "running")

		// --- VMI template metadata ---

		// Clear VMI annotations (KubeOVN provider annotations, etc.) but
		// preserve the hook sidecar annotation so clones use custom OVMF firmware.
		const hookAnnotation = "hooks.kubevirt.io/hookSidecars"
		vmiAnnotations, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "annotations")
		newVMIAnnotations := map[string]any{}
		if hook, ok := vmiAnnotations[hookAnnotation]; ok {
			newVMIAnnotations[hookAnnotation] = hook
		}
		_ = unstructured.SetNestedField(vm.Object, newVMIAnnotations, "spec", "template", "metadata", "annotations")

		// Update VMI labels.
		vmiLabels := map[string]any{
			LabelBuildID:      BuildID(build),
			LabelOS:           OSForShell(vmSpec.Communicator.Shell),
			clone.LabelVMName: vmSpec.Name,
			LabelBuild:        build.Name,
			LabelComponent:    "template",
		}
		_ = unstructured.SetNestedField(vm.Object, vmiLabels, "spec", "template", "metadata", "labels")

		// --- VMI template spec ---

		// Remove build-time pod affinity.
		unstructured.RemoveNestedField(vm.Object, "spec", "template", "spec", "affinity")

		// Rebuild volumes: boot disk → output PVC, additional data disks → their
		// own PVC, cloud-init → networkData only, drop ISOs.
		t.rebuildVolumes(vm, pvcName, BootDisk(vmSpec).Name)

		// Rebuild disks: keep data disks, drop ISOs/CDROMs and pin the boot
		// order to the boot disk so extra data disks can't steal the boot.
		t.rebuildDisks(vm, BootDisk(vmSpec).Name)

		// Replace networks with template placeholders.
		t.rebuildNetworks(vm, build, vmSpec)

		if err := t.Client.Update(ctx, vm); err != nil {
			// RetryOnConflict will retry IsConflict; other errors propagate.
			return err
		}
		converted = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("updating VM to template: %w", err)
	}
	return converted, nil
}

// rebuildVolumes replaces volumes on the VM:
//   - boot disk: source dataVolume → captured output PVC (pvcName)
//   - additional data disks: dataVolume ref → their own PVC (CDI names the PVC
//     after the DataVolume, so the claim name is the DataVolume name unchanged)
//   - cloudinit: strip userData, keep networkData (or remove if no NICs)
//   - ISOs: removed
func (t *TemplateProvisioner) rebuildVolumes(vm *unstructured.Unstructured, pvcName, bootDiskName string) {
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")

	var kept []any
	for _, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(vol, "name")

		// Drop ISO volumes (iso0, iso1, ...). The boot disk and additional data
		// disks are dataVolume-backed at this point, so only ISOs are PVC-backed.
		if _, hasPVC := vol["persistentVolumeClaim"]; hasPVC && name != bootDiskName {
			continue
		}

		// Disk volumes are dataVolume-backed during the build; swap each to a
		// persistentVolumeClaim reference for the template.
		if dv, hasDV := vol["dataVolume"]; hasDV {
			delete(vol, "dataVolume")
			if name == bootDiskName {
				// Boot disk → captured output PVC.
				vol["persistentVolumeClaim"] = map[string]any{
					"claimName": pvcName,
				}
			} else {
				// Additional data disk → its own blank DV-backed PVC. The DV
				// is not captured to an output copy, so reuse its name (CDI
				// creates a same-named PVC) to keep the disk's contents.
				dvName := ""
				if dvMap, ok := dv.(map[string]any); ok {
					dvName, _, _ = unstructured.NestedString(dvMap, "name")
				}
				vol["persistentVolumeClaim"] = map[string]any{
					"claimName": dvName,
				}
			}
			kept = append(kept, vol)
			continue
		}

		// Cloud-init: strip userData, keep networkData.
		if ciData, hasCI := vol["cloudInitNoCloud"]; hasCI {
			ci, ok := ciData.(map[string]any)
			if !ok {
				continue
			}
			delete(ci, "userData")
			// If no networkData remains, drop the entire cloud-init volume.
			if _, hasND := ci["networkData"]; !hasND {
				continue
			}
			vol["cloudInitNoCloud"] = ci
			kept = append(kept, vol)
			continue
		}

		// Keep anything else (e.g. extra data disks).
		kept = append(kept, vol)
	}

	_ = unstructured.SetNestedSlice(vm.Object, kept, "spec", "template", "spec", "volumes")
}

// rebuildDisks removes ISO cdrom entries, drops disks whose volumes were
// removed, and pins bootOrder=1 on the boot disk (clearing it elsewhere) so
// the firmware boots the OS disk even when extra data disks are attached.
func (t *TemplateProvisioner) rebuildDisks(vm *unstructured.Unstructured, bootDiskName string) {
	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")

	// Collect remaining volume names for reference.
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	volNames := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		if vol, ok := v.(map[string]any); ok {
			if name, _, _ := unstructured.NestedString(vol, "name"); name != "" {
				volNames[name] = true
			}
		}
	}

	var kept []any
	for _, d := range disks {
		disk, ok := d.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(disk, "name")

		// Drop disks whose volume was removed (ISOs).
		if !volNames[name] {
			continue
		}

		// Pin the boot order to the boot disk; clear it on every other
		// disk so an attached data disk (e.g. a usb drive) can't be picked
		// first and drop the firmware into the EFI shell.
		if name == bootDiskName {
			disk["bootOrder"] = int64(1)
		} else {
			delete(disk, "bootOrder")
		}

		kept = append(kept, disk)
	}

	_ = unstructured.SetNestedSlice(vm.Object, kept, "spec", "template", "spec", "domain", "devices", "disks")
}

// rebuildNetworks resets both the networks and interfaces arrays on a
// template VM to match the BASE spec's NIC list. Downstream builds may
// rename NICs via buildOverrides (e.g. eth0 -> nic1), which mutates the
// running VMI but must not bake into the template - clones inherit from
// the base, so the template needs the base names. Rebuilding only one
// array would leave them out of sync and KubeVirt's validator rejects
// the update ("interfaces[N].name 'X' not found" in networks).
func (t *TemplateProvisioner) rebuildNetworks(vm *unstructured.Unstructured, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) {
	if vmSpec == nil || len(vmSpec.NICs) == 0 {
		return // pod network with masquerade — no changes needed
	}

	networks := make([]any, 0, len(vmSpec.NICs))
	interfaces := make([]any, 0, len(vmSpec.NICs))
	for _, nic := range vmSpec.NICs {
		networks = append(networks, map[string]any{
			"name": nic.Name,
			"multus": map[string]any{
				"networkName": fmt.Sprintf("__template__/%s-nad", nic.Subnet),
			},
		})
		iface := map[string]any{
			"name": nic.Name,
		}
		// Bake the same binding the live VM uses so clones inherit it:
		// managedTap (no virt-launcher DHCP) for unmanaged subnets, core
		// bridge binding otherwise. See vm.go and featuregates.go.
		if subnetIsUnmanaged(build, nic.Subnet) {
			iface["binding"] = map[string]any{"name": kubevirt.UnmanagedBindingName}
		} else {
			iface["bridge"] = map[string]any{}
		}
		if nic.Model != "" {
			iface["model"] = nic.Model
		}
		if nic.MAC != "" {
			iface["macAddress"] = nic.MAC
		}
		if nic.Slot != 0 {
			iface["pciAddress"] = pciAddressForSlot(nic.Slot)
		}
		interfaces = append(interfaces, iface)
	}
	_ = unstructured.SetNestedSlice(vm.Object, networks, "spec", "template", "spec", "networks")
	_ = unstructured.SetNestedSlice(vm.Object, interfaces, "spec", "template", "spec", "domain", "devices", "interfaces")
}

// buildNetworkTopology serializes the build's network topology to JSON for
// embedding as an annotation on template VMs.
func (t *TemplateProvisioner) buildNetworkTopology(build *v1alpha1.VirtualMachineBuild) string {
	if build.Spec.Network == nil || len(build.Spec.Network.Subnets) == 0 {
		return ""
	}

	topo := clone.NetworkTopology{
		VMNICs: make(map[string][]clone.TopologyNIC),
	}

	for _, vpc := range build.Spec.Network.VPCs {
		topo.VPCs = append(topo.VPCs, clone.TopologyVPC{
			Name:     vpc.Name,
			Internet: vpc.Internet,
		})
	}
	for _, subnet := range build.Spec.Network.Subnets {
		topo.Subnets = append(topo.Subnets, clone.TopologySubnet{
			Name:      subnet.Name,
			VPC:       subnet.VPC,
			CIDR:      subnet.CIDR,
			DHCP:      subnet.DHCP,
			DNS:       subnet.DNS,
			Unmanaged: subnet.Unmanaged,
		})
	}
	// Use base spec NICs, not effectiveVMNICs — clones inherit this topology.
	for _, vm := range build.Spec.VMs {
		var nics []clone.TopologyNIC
		for _, nic := range vm.NICs {
			nics = append(nics, clone.TopologyNIC{
				Name:   nic.Name,
				Subnet: nic.Subnet,
				IP:     nic.IP,
				MAC:    nic.MAC,
			})
		}
		if len(nics) > 0 {
			topo.VMNICs[vm.Name] = nics
		}
	}

	data, err := json.Marshal(topo)
	if err != nil {
		return ""
	}
	return string(data)
}
