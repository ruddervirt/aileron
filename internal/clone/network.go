package clone

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/ruddervirt/aileron/internal/kubevirt"
	"github.com/ruddervirt/aileron/internal/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// AnnotationNetworkTopology is the annotation key for the network topology
	// stored on template VMs. The value is a JSON-encoded NetworkTopology.
	AnnotationNetworkTopology = "ruddervirt.io/network-topology"

	// LabelVMName records the short VM name (from BuildVM.spec.name) on
	// per-VM build resources — VMs, DVs, coordinator jobs, capture jobs,
	// template VMs. The clone logic reads this label off template VMs to
	// derive clone-side names without parsing the template VM's full name,
	// decoupling clone naming from the build layer's naming scheme. Lives
	// in the clone package because it's a cross-layer contract consumed
	// here; importing it from build would create a cycle (build → clone).
	LabelVMName = "ruddervirt.io/vm"

	// LabelBuildID is the canonical namespace-level identifier for both
	// build VMs (vm-xxx) and clone VMs (ns-xxx). Duplicated from
	// build.LabelBuildID to avoid the build → clone import cycle.
	LabelBuildID = "ruddervirt.io/build-id"

	// templateNADPlaceholder is the namespace placeholder used in template VM
	// multus network references. The clone controller replaces this with the
	// actual clone namespace and resource names.
	templateNADPlaceholder = "__template__"
)

// PreferredPodAffinity returns a KubeVirt-compatible affinity map that
// tells the scheduler to prefer co-locating pods with the same label value
// on the same node. Used by both build VMs and clone VMs so that VMs in
// the same build/clone land on the same host for lower inter-VM latency
// and better storage locality.
//
// The affinity is "preferred" (not required), so the scheduler will still
// place VMs on different nodes if a single node doesn't have enough resources.
func PreferredPodAffinity(labelKey, labelValue string) map[string]any {
	return map[string]any{
		"podAffinity": map[string]any{
			"preferredDuringSchedulingIgnoredDuringExecution": []any{
				map[string]any{
					"weight": int64(100),
					"podAffinityTerm": map[string]any{
						"labelSelector": map[string]any{
							"matchLabels": map[string]any{
								labelKey: labelValue,
							},
						},
						"topologyKey": "kubernetes.io/hostname",
					},
				},
			},
		},
	}
}

// NetworkTopology captures the logical network topology of a build.
// It is serialized as JSON into an annotation on template VMs so the clone
// controller can recreate the network infrastructure.
type NetworkTopology struct {
	VPCs    []TopologyVPC            `json:"vpcs,omitempty"`
	Subnets []TopologySubnet         `json:"subnets,omitempty"`
	VMNICs  map[string][]TopologyNIC `json:"vmNics"` // keyed by template VM name
}

// TopologyVPC is a logical VPC definition.
type TopologyVPC struct {
	Name     string `json:"name"`
	Internet bool   `json:"internet"`
}

// TopologySubnet is a logical subnet definition.
type TopologySubnet struct {
	Name string `json:"name"`
	VPC  string `json:"vpc"`
	CIDR string `json:"cidr"`
	// DHCP indicates whether DHCP is enabled. Pointer so the absence of a
	// value in older topology annotations is distinguishable from explicit false.
	DHCP *bool  `json:"dhcp,omitempty"`
	DNS  string `json:"dns,omitempty"`
	// Unmanaged delegates DHCP to a guest on this L2 segment. The clone
	// controller re-derives the operator-internal OVN flags from this.
	Unmanaged bool `json:"unmanaged,omitempty"`
}

// TopologyNIC is a logical NIC assignment for a VM.
type TopologyNIC struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
	IP     string `json:"ip,omitempty"`
	MAC    string `json:"mac,omitempty"`
}

// ExtractNetworkTopology reads the network topology annotation from a template VM.
// Returns nil if no annotation is present.
func ExtractNetworkTopology(templateVM *unstructured.Unstructured) (*NetworkTopology, error) {
	annotations := templateVM.GetAnnotations()
	if annotations == nil {
		return nil, nil
	}
	raw, ok := annotations[AnnotationNetworkTopology]
	if !ok || raw == "" {
		return nil, nil
	}
	var topo NetworkTopology
	if err := json.Unmarshal([]byte(raw), &topo); err != nil {
		return nil, fmt.Errorf("parsing network topology annotation: %w", err)
	}
	return &topo, nil
}

// RewireVMNetworks replaces placeholder NAD references in a cloned VM with
// real clone-scoped network resource names, and sets the KubeOVN pod annotations.
//
// resourcePrefix is used to build actual resource names: {prefix}-{logicalSubnet}.
// cloneNS is the namespace where NADs live.
func RewireVMNetworks(vm *unstructured.Unstructured, topo *NetworkTopology, resourcePrefix, cloneNS string) error {
	if topo == nil {
		return nil
	}

	// topo.VMNICs is keyed by the short VM name (spec.vms[].name) stored
	// on the template VM as the ruddervirt.io/vm label. Reading the label keeps
	// this lookup independent of the template VM's full name format.
	vmShortName := vm.GetLabels()[LabelVMName]
	if vmShortName == "" {
		return nil
	}
	nics, ok := topo.VMNICs[vmShortName]
	if !ok || len(nics) == 0 {
		return nil
	}

	subnetCIDRs := make(map[string]string)
	subnetUnmanaged := make(map[string]bool)
	for _, s := range topo.Subnets {
		subnetCIDRs[s.Name] = s.CIDR
		subnetUnmanaged[s.Name] = s.Unmanaged
	}

	// Rewrite networks array: replace placeholder NAD refs.
	networks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "networks")
	for i, netRaw := range networks {
		netMap, ok := netRaw.(map[string]any)
		if !ok {
			continue
		}
		multus, ok := netMap["multus"].(map[string]any)
		if !ok {
			continue
		}
		networkName, _ := multus["networkName"].(string)
		if !strings.HasPrefix(networkName, templateNADPlaceholder+"/") {
			continue
		}

		// Extract logical NAD name from placeholder: __template__/{logicalSubnet}-nad
		logicalNAD := strings.TrimPrefix(networkName, templateNADPlaceholder+"/")
		// logicalNAD is like "default-nad", "internal-nad"
		logicalSubnet := strings.TrimSuffix(logicalNAD, "-nad")

		actualSubnetName := network.SubnetName(resourcePrefix, logicalSubnet)
		actualNADName := actualSubnetName + "-nad"

		multus["networkName"] = fmt.Sprintf("%s/%s", cloneNS, actualNADName)
		netMap["multus"] = multus
		networks[i] = netMap
	}
	if err := unstructured.SetNestedSlice(vm.Object, networks, "spec", "template", "spec", "networks"); err != nil {
		return fmt.Errorf("setting networks: %w", err)
	}

	// Rewrite interface bindings for unmanaged NICs to the managedTap plugin.
	if err := rewriteUnmanagedBindings(vm, nics, subnetUnmanaged); err != nil {
		return err
	}

	// Build KubeOVN pod annotations for each NIC.
	annotations, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "annotations")
	if annotations == nil {
		annotations = make(map[string]string)
	}

	for _, nic := range nics {
		actualSubnetName := network.SubnetName(resourcePrefix, nic.Subnet)
		actualNADName := actualSubnetName + "-nad"
		provider := fmt.Sprintf("%s.%s.ovn", actualNADName, cloneNS)

		annotations[provider+".kubernetes.io/logical_switch"] = actualSubnetName
		if nic.IP != "" {
			annotations[provider+".kubernetes.io/ip_address"] = nic.IP
		}
		if nic.MAC != "" {
			annotations[provider+".kubernetes.io/mac_address"] = nic.MAC
		}

		// Add default route via subnet gateway. Skipped for unmanaged subnets:
		// virt-launcher serves no DHCP there, so the guest gateway VM provides
		// the route via its own DHCP.
		if cidr, ok := subnetCIDRs[nic.Subnet]; ok && !subnetUnmanaged[nic.Subnet] {
			gw := subnetGateway(cidr)
			if gw != "" {
				annotations[provider+".kubernetes.io/routes"] = fmt.Sprintf(`[{"gw":"%s"}]`, gw)
			}
		}
	}

	// Set annotations as map[string]any for unstructured.
	annotationsIface := make(map[string]any, len(annotations))
	for k, v := range annotations {
		annotationsIface[k] = v
	}
	if err := unstructured.SetNestedField(vm.Object, annotationsIface, "spec", "template", "metadata", "annotations"); err != nil {
		return fmt.Errorf("setting pod annotations: %w", err)
	}

	// Regenerate cloud-init networkData for DHCP on all bridge NICs.
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for i, volRaw := range volumes {
		volMap, ok := volRaw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := volMap["name"].(string)
		if name != "cloudinit" {
			continue
		}
		ci, ok := volMap["cloudInitNoCloud"].(map[string]any)
		if !ok {
			continue
		}
		// Regenerate networkData with DHCP for all NICs.
		var nd strings.Builder
		nd.WriteString("version: 2\nethernets:\n")
		for j := range nics {
			ifName := fmt.Sprintf("enp%ds0", j+1)
			fmt.Fprintf(&nd, "  %s:\n    dhcp4: true\n", ifName)
		}
		ci["networkData"] = nd.String()
		volMap["cloudInitNoCloud"] = ci
		volumes[i] = volMap
	}
	if err := unstructured.SetNestedSlice(vm.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		return fmt.Errorf("setting volumes: %w", err)
	}

	return nil
}

// rewriteUnmanagedBindings switches the interface binding to the managedTap
// plugin (no virt-launcher DHCP) for every NIC on an unmanaged subnet, so the
// guest gateway VM owns DHCP. New templates already bake this (see
// build/template.go rebuildNetworks); this pass also upgrades clones whose
// template was built by an older operator that wrote `bridge: {}` for every NIC.
func rewriteUnmanagedBindings(vm *unstructured.Unstructured, nics []TopologyNIC, subnetUnmanaged map[string]bool) error {
	nicUnmanaged := make(map[string]bool, len(nics))
	for _, nic := range nics {
		nicUnmanaged[nic.Name] = subnetUnmanaged[nic.Subnet]
	}

	ifaces, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	if len(ifaces) == 0 {
		return nil
	}
	for i, ifaceRaw := range ifaces {
		iface, ok := ifaceRaw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := iface["name"].(string)
		if !nicUnmanaged[name] {
			continue
		}
		delete(iface, "bridge")
		iface["binding"] = map[string]any{"name": kubevirt.UnmanagedBindingName}
		ifaces[i] = iface
	}
	if err := unstructured.SetNestedSlice(vm.Object, ifaces, "spec", "template", "spec", "domain", "devices", "interfaces"); err != nil {
		return fmt.Errorf("setting interfaces: %w", err)
	}
	return nil
}

// subnetGateway computes the first usable IP in a CIDR (KubeOVN default gateway).
func subnetGateway(cidr string) string {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	gw := ip.Mask(ipNet.Mask).To4()
	if gw == nil {
		return ""
	}
	gw[3]++
	return gw.String()
}
