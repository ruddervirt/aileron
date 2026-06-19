package build

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/clone"
	"github.com/ruddervirt/aileron/internal/kubevirt"
	"github.com/ruddervirt/aileron/internal/network"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VMBooter handles the Booting phase for a single VM: creating an ephemeral
// KubeVirt VM with the imported source disk, cloud-init, and KubeOVN NIC annotations.
type VMBooter struct {
	Client       client.Client
	SSHPublicKey []byte // Injected into cloud-init for provisioning access.
}

func (v *VMBooter) HandleVM(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) (v1alpha1.VMPhase, error) {
	vmName := BuildNameForBuildVM(BuildID(build), vmSpec.Name)
	buildNS := BuildNS(build)

	// If EFI firmware is requested, ensure PVC/Job/ConfigMap exist and are ready.
	if vmSpec.EFIFirmware != nil {
		if err := EnsureEFIFirmware(ctx, v.Client, build, vmSpec); err != nil {
			if errors.IsNotFound(err) {
				return v1alpha1.VMPhaseFailed, fmt.Errorf("ensuring EFI firmware: %w", err)
			}
			return v1alpha1.VMPhaseBooting, fmt.Errorf("ensuring EFI firmware: %w", err)
		}
		ready, err := IsEFIFirmwareReady(ctx, v.Client, build, vmSpec)
		if err != nil {
			return v1alpha1.VMPhaseFailed, fmt.Errorf("checking EFI firmware readiness: %w", err)
		}
		if !ready {
			return v1alpha1.VMPhaseBooting, nil
		}
	}

	// Check if VM already exists.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
	})
	err := v.Client.Get(ctx, types.NamespacedName{Name: vmName, Namespace: buildNS}, existing)
	if err == nil {
		// Validate the existing VM matches the desired spec. If cloud-init
		// presence drifted (e.g. created by an older version), delete and recreate.
		if v.vmSpecDrifted(existing, vmSpec) {
			if delErr := v.Client.Delete(ctx, existing); delErr != nil && !errors.IsNotFound(delErr) {
				return v1alpha1.VMPhaseFailed, fmt.Errorf("replacing drifted VM: %w", delErr)
			}
			// Fall through to recreate with correct spec.
		} else {
			vmStatus.VMName = vmName
			state, msg, err := v.checkVMI(ctx, vmName, buildNS)
			if err != nil {
				return v1alpha1.VMPhaseBooting, fmt.Errorf("checking VMI readiness: %w", err)
			}
			switch state {
			case vmiRunning:
				if len(vmSpec.BootCommand) > 0 {
					return v1alpha1.VMPhaseBootCommand, nil
				}
				return v1alpha1.VMPhaseProvisioning, nil
			case vmiFailed:
				return v1alpha1.VMPhaseFailed, fmt.Errorf("VM %s failed to boot: %s", vmSpec.Name, msg)
			default:
				return v1alpha1.VMPhaseBooting, nil
			}
		}
	}
	if !errors.IsNotFound(err) {
		return v1alpha1.VMPhaseBooting, fmt.Errorf("checking VM: %w", err)
	}

	vm, err := v.buildVM(build, vmSpec)
	if err != nil {
		return v1alpha1.VMPhaseFailed, fmt.Errorf("building VM spec: %w", err)
	}

	if err := v.Client.Create(ctx, vm); err != nil {
		if errors.IsAlreadyExists(err) {
			vmStatus.VMName = vmName
			return v1alpha1.VMPhaseBooting, nil
		}
		return v1alpha1.VMPhaseFailed, fmt.Errorf("creating VM: %w", err)
	}

	vmStatus.VMName = vmName
	return v1alpha1.VMPhaseBooting, nil
}

// vmiState represents the observed state of a VirtualMachineInstance.
type vmiState int

const (
	vmiPending vmiState = iota
	vmiRunning
	vmiFailed
)

func (v *VMBooter) checkVMI(ctx context.Context, name, namespace string) (vmiState, string, error) {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance",
	})
	err := v.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, vmi)
	if err != nil {
		if errors.IsNotFound(err) {
			return vmiPending, "", nil
		}
		return vmiPending, "", err
	}
	phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
	if phase == PhaseRunning {
		return vmiRunning, "", nil
	}
	if phase == PhaseFailed || phase == "Unknown" {
		return vmiFailed, fmt.Sprintf("VMI phase: %s", phase), nil
	}

	// Check VMI and VM conditions for sync failures. KubeVirt surfaces
	// libvirt config rejections (e.g. duplicate drive addresses) as
	// Synchronized=False with the libvirt error in the message field.
	// Historically this code matched only reason=SyncFailed, but real
	// failures come back with reasons like "Synchronizing with the Domain
	// failed." — hence the looser check. The Synchronized condition lives
	// on the VirtualMachine in current KubeVirt versions, not the VMI, so
	// fetch both and inspect together.
	if state, msg := syncFailureFromConditions(vmi.Object, "VMI"); state == vmiFailed {
		return state, msg, nil
	}
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
	})
	if err := v.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, vm); err == nil {
		if state, msg := syncFailureFromConditions(vm.Object, "VM"); state == vmiFailed {
			return state, msg, nil
		}
	}

	// Check the virt-launcher pod for container failures (ImagePullBackOff, CrashLoopBackOff, etc).
	if msg, failed := v.checkLauncherPod(ctx, name, namespace); failed {
		return vmiFailed, msg, nil
	}

	return vmiPending, "", nil
}

// syncFailureFromConditions inspects a KubeVirt resource's status.conditions
// for a Synchronized=False entry that signals a permanent configuration
// rejection from libvirt (rather than a transient state). A non-empty
// message is required to filter out brief False windows during normal
// VMI initialization. The kind argument is prefixed onto the returned
// message so the caller knows whether the VM or VMI surfaced the error.
func syncFailureFromConditions(obj map[string]any, kind string) (vmiState, string) {
	conditions, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(cond, "type")
		condStatus, _, _ := unstructured.NestedString(cond, "status")
		msg, _, _ := unstructured.NestedString(cond, "message")
		if condType == "Synchronized" && condStatus == "False" && msg != "" {
			return vmiFailed, fmt.Sprintf("%s Synchronized=False: %s", kind, msg)
		}
	}
	return vmiPending, ""
}

// checkLauncherPod inspects the virt-launcher pod's container statuses for
// terminal failures like ImagePullBackOff or CrashLoopBackOff.
func (v *VMBooter) checkLauncherPod(ctx context.Context, vmiName, namespace string) (string, bool) {
	podList := &corev1.PodList{}
	if err := v.Client.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{"kubevirt.io/domain": vmiName},
	); err != nil || len(podList.Items) == 0 {
		return "", false
	}

	pod := &podList.Items[0]
	allStatuses := append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...)
	for _, cs := range allStatuses {
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			switch reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				return fmt.Sprintf("container %s: %s: %s", cs.Name, reason, cs.State.Waiting.Message), true
			case "CrashLoopBackOff":
				return fmt.Sprintf("container %s: %s: %s", cs.Name, reason, cs.State.Waiting.Message), true
			}
		}
	}
	return "", false
}

// cpuCores derives the guest's whole vCPU core count from a CPU quantity,
// rounding up (a guest cannot have a fractional core) with a minimum of 1.
// This decouples the guest topology from the (possibly fractional) pod CPU
// request so idle VMs can overcommit: e.g. "100m" -> 1 core, "1.5" -> 2 cores.
func cpuCores(q resource.Quantity) int64 {
	cores := (q.MilliValue() + 999) / 1000
	if cores < 1 {
		return 1
	}
	return cores
}

func (v *VMBooter) buildVM(build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) (*unstructured.Unstructured, error) {
	vmName := BuildNameForBuildVM(BuildID(build), vmSpec.Name)
	dvName := BuildNameForBuildVMDataVolume(BuildID(build), vmSpec.Name)
	buildID := BuildID(build)
	buildNS := BuildNS(build)

	// Resolve effective resources honoring buildOverrides. The base
	// vmSpec.Resources is what the template VM (and thus clones) will use.
	// cpu and memory are required by the API, so they are always set here.
	resources := effectiveVMResources(build, vmSpec)
	cores := cpuCores(resources.CPU)
	cpuRequest := resources.CPU.String()

	var cloudInitData map[string]any
	if vmSpec.CloudInit != nil {
		cloudInitData = BuildCloudInitData(build, vmSpec, v.SSHPublicKey)
	}

	// Build pod annotations for KubeOVN NIC wiring via NADs.
	podAnnotations := make(map[string]any)

	// Resolve effective NICs honoring buildOverrides. Base vmSpec.NICs is
	// what the template VM (and thus clones) inherits — see template.go.
	nics := effectiveVMNICs(build, vmSpec)

	// Build KubeVirt multi-NIC networks and interfaces.
	// All VMs use bridge NICs only (no masquerade).
	networks := make([]any, 0, len(nics))
	interfaces := make([]any, 0, len(nics))

	for _, nic := range nics {
		subnetName := buildSubnetName(buildID, nic.Subnet)
		nadName := subnetName + "-nad"
		// Effective (buildOverride-aware) flag: a segment overridden managed
		// for the build uses core bridge binding (virt-launcher DHCP) here,
		// even though the base spec is unmanaged and the template bakes
		// l2bridge for clones (see template.go rebuildNetworks).
		unmanaged := effectiveSubnetUnmanaged(build, nic.Subnet)

		networks = append(networks, map[string]any{
			"name": nic.Name,
			"multus": map[string]any{
				"networkName": fmt.Sprintf("%s/%s", buildNS, nadName),
			},
		})
		iface := map[string]any{
			"name": nic.Name,
		}
		// Unmanaged subnets are bare L2 cables: attach via the managedTap
		// binding plugin so virt-launcher does NOT run its in-pod DHCP server
		// and a guest gateway VM (e.g. pfSense) owns DHCP on the segment.
		// Managed subnets keep core bridge binding (virt-launcher serves the
		// kube-ovn IPAM address). See internal/kubevirt/featuregates.go.
		if unmanaged {
			iface["binding"] = map[string]any{"name": kubevirt.UnmanagedBindingName}
		} else {
			iface["bridge"] = map[string]any{}
		}
		if nic.Model != "" {
			iface["model"] = nic.Model
		}
		// Pin the MAC at the libvirt/QEMU level. We also keep the OVN pod
		// annotation below so the logical port's MAC stays in sync with
		// what the guest actually emits.
		if nic.MAC != "" {
			iface["macAddress"] = nic.MAC
		}
		// Pin the PCI slot when the spec asks for one, so the same
		// logical NIC keeps the same PCI address across base builds and
		// downstream builds that reuse the image. See VMNIC.Slot for why.
		if nic.Slot != 0 {
			iface["pciAddress"] = pciAddressForSlot(nic.Slot)
		}
		interfaces = append(interfaces, iface)

		provider := fmt.Sprintf("%s.%s.ovn", nadName, buildNS)
		podAnnotations[provider+".kubernetes.io/logical_switch"] = subnetName
		if nic.IP != "" {
			podAnnotations[provider+".kubernetes.io/ip_address"] = nic.IP
		}
		if nic.MAC != "" {
			podAnnotations[provider+".kubernetes.io/mac_address"] = nic.MAC
		}

		// Add default route via the subnet gateway so virt-launcher's DHCP
		// serves a gateway to the VM. Without this, secondary interfaces
		// have no default route and the VM gets no gateway via DHCP.
		// Skipped for unmanaged subnets: virt-launcher serves no DHCP there,
		// so the guest gateway VM provides the route via its own DHCP.
		if !unmanaged {
			if gw := subnetGateway(build, nic.Subnet); gw != "" {
				routes, _ := json.Marshal([]map[string]string{{"gw": gw}})
				podAnnotations[provider+".kubernetes.io/routes"] = string(routes)
			}
		}
	}

	// Build domain spec. cpuRequest mirrors the effective cpu quantity, so the
	// guest's cores and its scheduler footprint come from the same per-VM knob;
	// likewise the pod memory request is the VM's declared memory.
	memoryRequest := resources.Memory.String()

	domainSpec := map[string]any{
		"cpu": map[string]any{
			"cores": cores,
		},
		// pc-i440fx-rhel7.6.0 is the only i440fx variant RHEL's qemu-kvm
		// ships (frozen at 7.6.0; only q35 keeps getting newer revs). q35
		// would otherwise be the more modern default, but RHEL's qemu-kvm
		// gates isa-fdc off for q35 — the floppy controller is the only
		// reliable way to make Windows Setup pick up our Autounattend.xml
		// on A: before any install-ISO-bundled autounattend at D:+. We
		// can't use the bare alias "pc" here because KubeVirt's validator
		// prefix-matches the literal spec string against the allowlist
		// (q35*, pc-q35*, pc-i440fx*) without expanding qemu aliases.
		// Output disk images stay portable because virtio drivers (loaded
		// via the autounattend's DriverPaths) work across both chipsets.
		"machine": map[string]any{
			"type": "pc-i440fx-rhel7.6.0",
		},
		"resources": map[string]any{
			"requests": map[string]any{
				"cpu":    cpuRequest,
				"memory": memoryRequest,
			},
		},
		"devices":          v.buildDevices(interfaces, vmSpec),
		"logSerialConsole": true,
	}

	// Add EFI firmware config if requested. SMM is only needed for Secure
	// Boot, which i440fx doesn't support anyway — leave it off here.
	if vmSpec.EFIFirmware != nil {
		domainSpec["firmware"] = map[string]any{
			"bootloader": map[string]any{
				"efi": map[string]any{
					"secureBoot": vmSpec.EFIFirmware.SecureBoot,
				},
			},
		}
	}

	// Build VMI template annotations — add hook sidecars for EFI and/or floppy.
	vmiAnnotations := podAnnotations
	hookJSON, err := BuildHookSidecarsAnnotation(BuildID(build), vmSpec)
	if err != nil {
		return nil, fmt.Errorf("building hook sidecar annotation: %w", err)
	}
	if hookJSON != "" {
		vmiAnnotations["hooks.kubevirt.io/hookSidecars"] = hookJSON
	}

	// pc-i440fx has no PCIe complex, so KubeVirt 1.5+'s hotplug port
	// pre-allocator (`virDomainPCIAddressSetGrow`) fails the domain define
	// with "a PCIe slot is needed". This annotation skips that pre-allocation.
	vmiAnnotations["kubevirt.io/placePCIDevicesOnRootComplex"] = "true"

	// Propagate the origin attribution so an external watcher can correlate
	// VM lifecycle events back to the originating request.
	source := build.GetAnnotations()[v1alpha1.AnnotationOrigin]
	if source != "" {
		vmiAnnotations[v1alpha1.AnnotationOrigin] = source
	}

	vmAnnotations := map[string]any{}
	if source != "" {
		vmAnnotations[v1alpha1.AnnotationOrigin] = source
	}

	vm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":        vmName,
				"namespace":   buildNS,
				"annotations": vmAnnotations,
				"labels": map[string]any{
					LabelBuildID:        buildID,
					LabelOS:             OSForShell(vmSpec.Communicator.Shell),
					LabelBuild:          build.Name,
					LabelBuildNamespace: build.Namespace,
					LabelVM:             vmSpec.Name,
				},
			},
			"spec": map[string]any{
				"runStrategy": "RerunOnFailure",
				"template": map[string]any{
					"metadata": map[string]any{
						"annotations": vmiAnnotations,
						"labels": map[string]any{
							LabelBuildID:        buildID,
							LabelOS:             OSForShell(vmSpec.Communicator.Shell),
							LabelBuild:          build.Name,
							LabelBuildNamespace: build.Namespace,
							LabelVM:             vmSpec.Name,
						},
					},
					"spec": map[string]any{
						"affinity":  clone.PreferredPodAffinity(LabelBuildID, buildID),
						"dnsPolicy": "None",
						"dnsConfig": map[string]any{
							"nameservers": stringsToAny(buildVMDNSServers(build, vmSpec)),
						},
						"domain":   domainSpec,
						"networks": networks,
						"volumes":  v.buildVolumes(dvName, cloudInitData, vmSpec, buildID),
					},
				},
			},
		},
	}

	return vm, nil
}

func (v *VMBooter) buildDevices(bridgeInterfaces []any, vmSpec *v1alpha1.BuildVM) map[string]any {
	vmDisks := DefaultDisks(vmSpec)
	blankSource := vmSpec.Source.Blank
	isoCount := len(vmSpec.ISOs)

	var disks []any
	for i, d := range vmDisks {
		bus := d.Bus
		if bus == "" {
			bus = "virtio"
		}
		disk := map[string]any{
			"name": d.Name,
			"disk": map[string]any{"bus": bus},
		}
		// Boot disk first, ISO second. The boot disk always gets bootOrder=1
		// so attached data disks (e.g. a usb drive) can't be picked first and
		// drop the firmware into the EFI shell. On a blank-source build the
		// disk starts empty, so OVMF falls through to the ISO (bootOrder=2,
		// set below) and runs the installer; after the installer writes a
		// bootloader, subsequent boots find it on the disk. We rely on the
		// cdrom being on virtio-scsi (set below) so the first-read latency
		// that previously broke fall-through on ATAPI/AHCI doesn't recur here.
		if i == 0 {
			disk["bootOrder"] = int64(1)
		}
		disks = append(disks, disk)
	}
	if vmSpec.CloudInit != nil {
		disks = append(disks, map[string]any{
			"name": "cloudinit",
			"disk": map[string]any{"bus": "virtio"},
		})
	}
	for i := range isoCount {
		cdrom := map[string]any{
			"name":  fmt.Sprintf("iso%d", i),
			"cdrom": map[string]any{"bus": "scsi"},
		}
		if blankSource && i == 0 {
			cdrom["bootOrder"] = int64(2)
		}
		disks = append(disks, cdrom)
	}
	// Floppy is NOT added to devices.disks — it's injected directly into the
	// libvirt XML via a sidecar hook because KubeVirt doesn't support floppy devices.

	return map[string]any{
		"disks":                   disks,
		"interfaces":              bridgeInterfaces,
		"autoattachSerialConsole": true,
		// Every VM gets a persistent vTPM. KubeVirt backs the TPM state with a
		// per-VM `persistent-state-for-<vm>` PVC provisioned from the KubeVirt
		// CR's vmStateStorageClass (ensured by internal/kubevirt's
		// EnsureFeatureGates), so TPM-sealed secrets — e.g. Windows 11 /
		// BitLocker — survive reboots. We add it on the build VM and it
		// propagates unchanged into the template (convertToTemplate preserves
		// domain.devices) and into every clone (which deep-copies the template).
		"tpm": map[string]any{
			"persistent": true,
		},
		"inputs": []any{
			map[string]any{
				"type": "tablet",
				"bus":  "usb",
				"name": "tablet0",
			},
		},
	}
}

func (v *VMBooter) buildVolumes(dvName string, cloudInitData map[string]any, vmSpec *v1alpha1.BuildVM, buildID string) []any {
	vmDisks := DefaultDisks(vmSpec)
	var volumes []any
	for i, d := range vmDisks {
		name := DiskDVName(buildID, vmSpec.Name, i, d.Name)
		if i == 0 {
			name = dvName // boot disk uses the source DV name
		}
		volumes = append(volumes, map[string]any{
			"name": d.Name,
			"dataVolume": map[string]any{
				"name": name,
			},
		})
	}
	if cloudInitData != nil {
		volumes = append(volumes, map[string]any{
			"name":             "cloudinit",
			"cloudInitNoCloud": cloudInitData,
		})
	}
	for i := range vmSpec.ISOs {
		// Mount the per-build clone, NOT the shared cache. The cache is RWO
		// in the common case (block CSI), so attaching it directly serializes
		// concurrent builds. The clone is created and waited on by
		// ISOImporter.HandleISOs before this VM is built.
		volumes = append(volumes, map[string]any{
			"name": fmt.Sprintf("iso%d", i),
			"persistentVolumeClaim": map[string]any{
				"claimName": ISOCloneDVName(buildID, vmSpec.Name, i),
			},
		})
	}
	// Floppy is NOT a KubeVirt volume — KubeVirt runs host-disk preparation
	// on all PVC volumes which fails due to space checks. Instead, the floppy
	// image is written to the EFI vars PVC and injected by the sidecar hook
	// via the shared compute path at /var/run/efivars/floppy.img.
	return volumes
}

// BuildCloudInitData builds the cloud-init data map for a VM, injecting the SSH public key.
func BuildCloudInitData(build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, sshPublicKey []byte) map[string]any {
	userData := "#cloud-config\n"
	if vmSpec.CloudInit != nil && vmSpec.CloudInit.UserData != "" {
		userData = vmSpec.CloudInit.UserData
	}

	// Inject the SSH public key for provisioning access.
	if len(sshPublicKey) > 0 {
		userData = InjectSSHKeyIntoCloudInit(userData, sshPublicKey)
	} else if !strings.Contains(userData, "ssh_authorized_keys") {
		userData += "\nssh_authorized_keys: []\n"
	}

	result := map[string]any{
		"userData": userData,
	}
	nics := effectiveVMNICs(build, vmSpec)
	if vmSpec.CloudInit != nil && vmSpec.CloudInit.NetworkData != "" {
		result["networkData"] = vmSpec.CloudInit.NetworkData
	} else if len(nics) > 0 {
		result["networkData"] = generateNetworkData(nics)
	}
	return result
}

// generateNetworkData produces a cloud-init network config v2 YAML that
// enables DHCP on all ethernet interfaces.
// All VMs use bridge NICs starting at enp1s0 (PCI slot 1).
func generateNetworkData(nics []v1alpha1.VMNIC) string {
	var b strings.Builder
	b.WriteString("version: 2\nethernets:\n")
	for i := range nics {
		fmt.Fprintf(&b, "  enp%ds0:\n    dhcp4: true\n", i+1)
	}
	return b.String()
}

// subnetGateway returns the gateway IP for a subnet by computing the first
// usable IP in the CIDR (e.g. 10.0.0.1 for 10.0.0.0/24). This matches
// KubeOVN's default gateway assignment.
func subnetGateway(build *v1alpha1.VirtualMachineBuild, subnetName string) string {
	if build.Spec.Network == nil {
		return ""
	}
	for _, s := range build.Spec.Network.Subnets {
		if s.Name == subnetName {
			cidr := s.CIDR
			if cidr == "" {
				cidr = "10.0.0.0/24"
			}
			ip, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				return ""
			}
			// Gateway = network address + 1.
			gw := ip.Mask(ipNet.Mask).To4()
			if gw == nil {
				return ""
			}
			gw[3]++
			return gw.String()
		}
	}
	return ""
}

// buildVMDNSServers computes the DNS server list to lock onto the VMI's
// dnsConfig. Pinning these prevents virt-launcher's bridge-mode DHCP fallback
// (which uses the pod's own resolv.conf for any field the CNI IPAM result
// leaves blank) from ever propagating kube-dns or the cluster search domains
// into the guest. We derive the value from the VM's first NIC subnet using
// the same priority KubeOVN's dhcpV4Options follows so both paths agree.
// The CRD does not expose dnsPolicy/dnsConfig, so callers cannot override this.
func buildVMDNSServers(build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) []string {
	fallback := []string{"8.8.8.8"}
	nics := effectiveVMNICs(build, vmSpec)
	if build.Spec.Network == nil || len(nics) == 0 {
		return fallback
	}
	// Pick the first NIC's subnet for the DNS source.
	var subnet *v1alpha1.Subnet
	for _, nic := range nics {
		for i := range build.Spec.Network.Subnets {
			s := &build.Spec.Network.Subnets[i]
			if s.Name != nic.Subnet {
				continue
			}
			subnet = s
			break
		}
		if subnet != nil {
			break
		}
	}
	if subnet == nil {
		return fallback
	}
	internet := false
	for _, vpc := range build.Spec.Network.VPCs {
		if vpc.Name == subnet.VPC && effectiveVPCInternet(build, vpc) {
			internet = true
			break
		}
	}
	servers := network.SubnetDNSServers(subnet.CIDR, subnet.DNS, internet)
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

// pciAddressForSlot maps a VMNIC slot (validated 1-9) to a stable PCI BDF.
// The low slots on i440fx are claimed by libvirt for implicit devices
// (host bridge, ISA/IDE/USB, rootdisk, balloon), so NICs live at 0x11-0x19
// to keep them off that range and free of collisions.
func pciAddressForSlot(slot int) string {
	return fmt.Sprintf("0000:00:1%d.0", slot)
}

// vmSpecDrifted checks whether an existing VM's spec differs from the desired
// spec in ways that require recreation. Currently checks cloud-init volume presence.
func (v *VMBooter) vmSpecDrifted(existing *unstructured.Unstructured, vmSpec *v1alpha1.BuildVM) bool {
	volumes, _, _ := unstructured.NestedSlice(existing.Object, "spec", "template", "spec", "volumes")
	hasCloudInit := false
	for _, vol := range volumes {
		if m, ok := vol.(map[string]any); ok {
			if name, _, _ := unstructured.NestedString(m, "name"); name == "cloudinit" {
				hasCloudInit = true
				break
			}
		}
	}
	wantCloudInit := vmSpec.CloudInit != nil
	return hasCloudInit != wantCloudInit
}
