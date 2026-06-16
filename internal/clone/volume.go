package clone

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/network"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const EFIVarsVolumeName = "efivars"

// VolumeManager handles volume provisioning for clone operations.
type VolumeManager struct {
	Client client.Client
}

// CloneVMName derives the cloned VM's name from the cloneID and the short VM
// name (BuildVM.spec.name). The short name is read from the template VM's
// ruddervirt.io/vm label by the caller, so this function does no string parsing
// on the template's own name.
//
// Example: cloneID="cl-abc", vmShortName="builder" → "cl-abc-builder".
func CloneVMName(cloneID, vmShortName string) string {
	name := cloneID + "-" + vmShortName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// CloneDiskPVCName derives the destination PVC name for a cloned disk. It is
// unique per (clone, VM, volume) so a multi-disk VM does not collide every disk
// onto a single name — efivars and additional data disks all follow this scheme.
func CloneDiskPVCName(cloneID, vmShortName, volumeName string) string {
	name := fmt.Sprintf("%s-%s-%s", cloneID, vmShortName, volumeName)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// templateVMShortName returns the short VM name stored in the template VM's
// ruddervirt.io/vm label. Returns an empty string if the label is absent, which
// the caller should treat as a provisioning bug in the build layer.
func templateVMShortName(vm *unstructured.Unstructured) string {
	return vm.GetLabels()[LabelVMName]
}

// EnsureClonePVC creates a PVC from a snapshot directly in the clone namespace.
// Since template and clone share the same namespace, no cross-namespace PV
// transfer is needed.
func (v *VolumeManager) EnsureClonePVC(ctx context.Context, cloneID string, state *v1alpha1.CloneVolumeStatus, cloneNamespace string) (bool, error) {
	logger := log.FromContext(ctx)

	if state.PersistentVolumeClaimName == "" {
		if state.SourceVMShortName == "" {
			return false, fmt.Errorf("volume %s has no sourceVmShortName — template VM is missing the %s label",
				state.VolumeName, LabelVMName)
		}
		state.PersistentVolumeClaimName = CloneDiskPVCName(cloneID, state.SourceVMShortName, state.VolumeName)
	}

	pvcName := state.PersistentVolumeClaimName

	// Check if PVC already exists.
	existing := &corev1.PersistentVolumeClaim{}
	err := v.Client.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: cloneNamespace}, existing)
	if err == nil {
		if existing.Status.Phase == corev1.ClaimBound {
			state.Phase = v1alpha1.CloneVolumePhasePVCBound
			return true, nil
		}
		return false, nil
	}
	if !errors.IsNotFound(err) {
		return false, fmt.Errorf("checking clone PVC %s: %w", pvcName, err)
	}

	storageReq := state.RequestedStorage
	if storageReq == "" {
		storageReq = "10Gi"
	}

	apiGroup := "snapshot.storage.k8s.io"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: cloneNamespace,
			Labels: map[string]string{
				"ruddervirt.io/clone":     cloneID,
				"ruddervirt.io/source-vm": state.SourceVMName,
			},
			Annotations: map[string]string{
				"cdi.kubevirt.io/storage.contentType": "kubevirt",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(storageReq),
				},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     state.SnapshotName,
			},
		},
	}
	if state.StorageClassName != "" {
		pvc.Spec.StorageClassName = &state.StorageClassName
	}

	if err := v.Client.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating clone PVC: %w", err)
	}

	logger.Info("Created clone PVC from snapshot", "pvc", pvcName, "snapshot", state.SnapshotName)
	return false, nil
}

// RewireVMVolumes updates a cloned VM's volume references from DataVolume to PVC.
func RewireVMVolumes(vm *unstructured.Unstructured, volumeStates []v1alpha1.CloneVolumeStatus) error {
	vmName := vm.GetName()
	volumes, found, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		return nil
	}

	// Build lookup: volume name -> dest PVC name for this VM.
	pvcByVolume := make(map[string]string)
	for _, state := range volumeStates {
		if state.SourceVMName == vmName && state.PersistentVolumeClaimName != "" {
			pvcByVolume[state.VolumeName] = state.PersistentVolumeClaimName
		}
	}

	changed := false
	for i, vol := range volumes {
		volMap, ok := vol.(map[string]any)
		if !ok {
			continue
		}
		volName, _, _ := unstructured.NestedString(volMap, "name")
		destPVC, ok := pvcByVolume[volName]
		if !ok {
			continue
		}

		// Remove dataVolume reference and replace with persistentVolumeClaim.
		delete(volMap, "dataVolume")
		volMap["persistentVolumeClaim"] = map[string]any{
			"claimName": destPVC,
		}
		volumes[i] = volMap
		changed = true
	}

	if changed {
		if err := unstructured.SetNestedSlice(vm.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
			return fmt.Errorf("setting volumes on VM %s: %w", vmName, err)
		}
	}

	// Also handle dataVolumeTemplates — remove them since we use PVCs directly.
	unstructured.RemoveNestedField(vm.Object, "spec", "dataVolumeTemplates")

	return nil
}

// pinBootDiskOrder sets bootOrder=1 on the first hard disk and clears bootOrder
// on every other disk, so the firmware boots the OS disk regardless of how many
// data disks (e.g. usb drives) are attached. CDROMs are dropped during template
// conversion, so the first "disk"-type entry is the boot disk.
func pinBootDiskOrder(vm *unstructured.Unstructured) error {
	disks, found, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	if err != nil || !found {
		return err
	}

	pinned := false
	for i, d := range disks {
		disk, ok := d.(map[string]any)
		if !ok {
			continue
		}
		_, isDisk := disk["disk"]
		if isDisk && !pinned {
			disk["bootOrder"] = int64(1)
			pinned = true
		} else {
			delete(disk, "bootOrder")
		}
		disks[i] = disk
	}

	return unstructured.SetNestedSlice(vm.Object, disks, "spec", "template", "spec", "domain", "devices", "disks")
}

// ensureVirtualMachine creates a cloned VM in the destination namespace.
func ensureVirtualMachine(ctx context.Context, c client.Client, templateVM *unstructured.Unstructured, cloneID, cloneNamespace, source string, volumeStates []v1alpha1.CloneVolumeStatus, networkTopo *NetworkTopology, ageAnchor *metav1.Time) error {
	logger := log.FromContext(ctx)
	templateVMName := templateVM.GetName()
	vmShortName := templateVMShortName(templateVM)
	if vmShortName == "" {
		return fmt.Errorf("template VM %s is missing the %s label — clone cannot derive a target name",
			templateVMName, LabelVMName)
	}
	cloneVMName := CloneVMName(cloneID, vmShortName)

	// Check if VM already exists.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(vmGVK)
	err := c.Get(ctx, types.NamespacedName{Name: cloneVMName, Namespace: cloneNamespace}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("checking VM %s: %w", cloneVMName, err)
	}

	// Deep copy and sanitize the template VM.
	// Rewire volumes and networks BEFORE renaming so that lookups
	// by template VM name (state.SourceVMName, topo.VMNICs) still work.
	vm := templateVM.DeepCopy()
	vm.SetResourceVersion("")
	vm.SetUID("")
	vm.SetCreationTimestamp(metav1.Time{})
	vm.SetGeneration(0)
	vm.SetManagedFields(nil)
	delete(vm.Object, "status")

	// Remove the network topology annotation from the clone.
	annotations := vm.GetAnnotations()
	if annotations != nil {
		delete(annotations, AnnotationNetworkTopology)
		vm.SetAnnotations(annotations)
	}

	// Remove owner references.
	vm.SetOwnerReferences(nil)

	// Rewire volumes (uses templateVMName via state.SourceVMName for lookup).
	if err := RewireVMVolumes(vm, volumeStates); err != nil {
		return fmt.Errorf("rewiring volumes for VM %s: %w", cloneVMName, err)
	}

	// Rewire networks (uses templateVMName via vm.GetName() for topo.VMNICs lookup).
	if networkTopo != nil {
		if err := RewireVMNetworks(vm, networkTopo, cloneID, cloneNamespace); err != nil {
			return fmt.Errorf("rewiring networks for VM %s: %w", cloneVMName, err)
		}
	}

	// Unconditionally pin the boot order to the first (boot) disk. Templates
	// provisioned by an older operator have no bootOrder on any disk; that was
	// harmless with a single disk but lets an attached data disk (e.g. a usb
	// drive) be picked first, which drops the firmware into the EFI shell.
	if err := pinBootDiskOrder(vm); err != nil {
		return fmt.Errorf("pinning boot order on VM %s: %w", cloneVMName, err)
	}

	// Unconditionally clamp dnsPolicy/dnsConfig. The CRD does not expose these
	// fields, but a template VM could have inherited a leaky pod-DNS config from
	// an older operator version — overwrite regardless so virt-launcher's
	// bridge-mode DHCP fallback has no kube-dns to leak into the guest.
	if err := unstructured.SetNestedField(vm.Object, "None", "spec", "template", "spec", "dnsPolicy"); err != nil {
		return fmt.Errorf("setting dnsPolicy on VM %s: %w", cloneVMName, err)
	}
	if err := unstructured.SetNestedField(vm.Object,
		map[string]any{"nameservers": stringsToAny(cloneVMDNSServers(networkTopo, vmShortName))},
		"spec", "template", "spec", "dnsConfig",
	); err != nil {
		return fmt.Errorf("setting dnsConfig on VM %s: %w", cloneVMName, err)
	}

	// Merge VMI template annotations: always set the hotplug-suppression
	// annotation (i440fx has no PCIe; KubeVirt 1.5+'s hotplug port
	// pre-allocator fails domain define otherwise). Then conditionally add
	// the EFI hook sidecar annotation if the clone has its own EFI vars PVC.
	tmplAnn, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "annotations")
	annMap := make(map[string]any, len(tmplAnn)+2)
	for k, v := range tmplAnn {
		annMap[k] = v
	}
	annMap["kubevirt.io/placePCIDevicesOnRootComplex"] = "true"
	for _, state := range volumeStates {
		if state.VolumeName == EFIVarsVolumeName && state.SourceVMName == templateVMName && state.PersistentVolumeClaimName != "" {
			hookJSON, err := hookSidecarsJSON(state.PersistentVolumeClaimName)
			if err != nil {
				return fmt.Errorf("building hook sidecar annotation for VM %s: %w", cloneVMName, err)
			}
			annMap["hooks.kubevirt.io/hookSidecars"] = hookJSON
			break
		}
	}
	_ = unstructured.SetNestedField(vm.Object, annMap, "spec", "template", "metadata", "annotations")

	// NOW rename to clone-scoped name (after rewiring).
	vm.SetName(cloneVMName)
	vm.SetNamespace(cloneNamespace)

	// Replace labels on both VM metadata and pod template. Carry the
	// ruddervirt.io/os label across from the template so the grader can still
	// infer the serial-console protocol from a clone without looking up the
	// template VM.
	labels := map[string]string{
		LabelBuildID:              cloneID,
		"ruddervirt.io/clone":     cloneID,
		"ruddervirt.io/vm":        vmShortName,
		"ruddervirt.io/source-vm": templateVMName,
		"ruddervirt.io/component": "clone",
	}
	if osLabel := templateVM.GetLabels()["ruddervirt.io/os"]; osLabel != "" {
		labels["ruddervirt.io/os"] = osLabel
	}
	vm.SetLabels(labels)

	// Set the same labels on the VM's pod template so VMIs inherit them.
	podTemplateLabels := map[string]string{
		LabelBuildID:              cloneID,
		"ruddervirt.io/clone":     cloneID,
		"ruddervirt.io/vm":        vmShortName,
		"ruddervirt.io/component": "clone",
	}
	if osLabel := templateVM.GetLabels()["ruddervirt.io/os"]; osLabel != "" {
		podTemplateLabels["ruddervirt.io/os"] = osLabel
	}
	_ = unstructured.SetNestedStringMap(vm.Object, podTemplateLabels, "spec", "template", "metadata", "labels")

	// Propagate the origin attribution so an external watcher can correlate
	// VM lifecycle events back to the originating request.
	if source != "" {
		vmAnn := vm.GetAnnotations()
		if vmAnn == nil {
			vmAnn = make(map[string]string)
		}
		vmAnn[v1alpha1.AnnotationOrigin] = source
		vm.SetAnnotations(vmAnn)

		vmiAnn, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "annotations")
		annMap := make(map[string]any, len(vmiAnn)+1)
		for k, v := range vmiAnn {
			annMap[k] = v
		}
		annMap[v1alpha1.AnnotationOrigin] = source
		_ = unstructured.SetNestedField(vm.Object, annMap, "spec", "template", "metadata", "annotations")
	}

	// Stamp the inherited age anchor so the watchdog deletes this VM at the
	// same wall-clock time the predecessor's would have been deleted.
	if ageAnchor != nil {
		vmAnn := vm.GetAnnotations()
		if vmAnn == nil {
			vmAnn = make(map[string]string)
		}
		vmAnn["ruddervirt.io/age-anchor"] = ageAnchor.UTC().Format(time.RFC3339)
		vm.SetAnnotations(vmAnn)
	}

	// Cloned VMs are created halted. The user explicitly powers them on
	// via the KubeVirt start subresource once they're ready to use the clone.
	unstructured.RemoveNestedField(vm.Object, "spec", "running")
	if err := unstructured.SetNestedField(vm.Object, "Manual", "spec", "runStrategy"); err != nil {
		return err
	}

	// Prefer co-locating cloned VMs from the same clone on the same node.
	if err := unstructured.SetNestedField(vm.Object,
		PreferredPodAffinity("ruddervirt.io/clone", cloneID),
		"spec", "template", "spec", "affinity"); err != nil {
		return fmt.Errorf("setting pod affinity on VM %s: %w", cloneVMName, err)
	}

	if err := c.Create(ctx, vm); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating cloned VM %s: %w", cloneVMName, err)
	}

	logger.Info("Created cloned VM", "vm", cloneVMName, "namespace", cloneNamespace)
	return nil
}

// EnsureVMs creates all cloned VMs in the destination namespace. When
// ageAnchor is non-nil, the resolved RFC3339 timestamp is stamped as the
// ruddervirt.io/age-anchor annotation on each cloned VM so the watchdog
// uses it instead of the VM's own creationTimestamp for age checks.
func EnsureVMs(ctx context.Context, c client.Client, templateVMs []*unstructured.Unstructured, cloneID, cloneNamespace, source string, volumeStates []v1alpha1.CloneVolumeStatus, networkTopo *NetworkTopology, ageAnchor *metav1.Time) error {
	for _, vm := range templateVMs {
		if err := ensureVirtualMachine(ctx, c, vm, cloneID, cloneNamespace, source, volumeStates, networkTopo, ageAnchor); err != nil {
			return err
		}
	}
	return nil
}

// CheckVMsReady checks if all cloned VirtualMachine resources have been
// created. Clones are provisioned halted (runStrategy: Manual), so we do
// not wait for the VMIs to be running — that happens later when the user
// explicitly powers the VMs on.
func CheckVMsReady(ctx context.Context, c client.Client, templateVMs []*unstructured.Unstructured, cloneID, cloneNamespace string) ([]v1alpha1.ClonedVMStatus, bool, error) {
	allReady := true
	var statuses []v1alpha1.ClonedVMStatus

	vmGVK := schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
	}

	for _, templateVM := range templateVMs {
		vmShortName := templateVMShortName(templateVM)
		if vmShortName == "" {
			return nil, false, fmt.Errorf("template VM %s is missing the %s label",
				templateVM.GetName(), LabelVMName)
		}
		vmName := CloneVMName(cloneID, vmShortName)
		status := v1alpha1.ClonedVMStatus{Name: vmName}

		vm := &unstructured.Unstructured{}
		vm.SetGroupVersionKind(vmGVK)
		err := c.Get(ctx, types.NamespacedName{Name: vmName, Namespace: cloneNamespace}, vm)
		if err != nil {
			if errors.IsNotFound(err) {
				status.Message = "VM not yet created"
				allReady = false
			} else {
				return nil, false, fmt.Errorf("checking VM %s: %w", vmName, err)
			}
		} else {
			status.Ready = true
			status.Message = "halted; power on to start"
		}

		statuses = append(statuses, status)
	}

	return statuses, allReady, nil
}

// cloneVMDNSServers picks the DNS server list for a clone VM's dnsConfig,
// mirroring the policy used by the OVN subnet's dhcpV4Options so the two
// agree byte-for-byte. Mirrors buildVMDNSServers on the build side.
func cloneVMDNSServers(topo *NetworkTopology, vmShortName string) []string {
	fallback := []string{"8.8.8.8"}
	if topo == nil {
		return fallback
	}
	nics := topo.VMNICs[vmShortName]
	if len(nics) == 0 {
		return fallback
	}
	// Pick the first NIC's subnet for the DNS source.
	var sub *TopologySubnet
	for _, nic := range nics {
		for i := range topo.Subnets {
			s := &topo.Subnets[i]
			if s.Name != nic.Subnet {
				continue
			}
			sub = s
			break
		}
		if sub != nil {
			break
		}
	}
	if sub == nil {
		return fallback
	}
	internet := false
	for _, vpc := range topo.VPCs {
		if vpc.Name == sub.VPC && vpc.Internet {
			internet = true
			break
		}
	}
	servers := network.SubnetDNSServers(sub.CIDR, sub.DNS, internet)
	if len(servers) == 0 {
		return fallback
	}
	return servers
}

// stringsToAny converts a []string into a []any for unstructured fields.
func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func hookSidecarsJSON(pvcName string) (string, error) {
	img := os.Getenv("OPERATOR_IMAGE")
	if img == "" {
		img = "ghcr.io/ruddervirt/aileron:latest"
	}
	if idx := strings.LastIndex(img, ":"); idx > 0 {
		img = img[:idx] + "/sidecar" + img[idx:]
	} else {
		img += "/sidecar"
	}

	hooks := []map[string]any{{
		"args":  []string{"--version", "v1alpha2"},
		"image": img,
		"pvc": map[string]any{
			"name":              pvcName,
			"volumePath":        "/efivars",
			"sharedComputePath": "/var/run/efivars",
		},
	}}
	data, err := json.Marshal(hooks)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
