package clone

import (
	"testing"

	"github.com/ruddervirt/aileron/internal/kubevirt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestRewireVMNetworksUnmanagedBinding asserts that cloning rewrites unmanaged
// NICs to the managedTap binding plugin (no virt-launcher DHCP) and suppresses
// their default-route annotation, while managed NICs keep core bridge binding
// and their route. This also covers upgrading a template baked by an older
// operator that wrote `bridge: {}` for every NIC.
func TestRewireVMNetworksUnmanagedBinding(t *testing.T) {
	vm := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{},
				"spec": map[string]any{
					"networks": []any{
						map[string]any{"name": "nic1", "multus": map[string]any{"networkName": templateNADPlaceholder + "/mgmt-nad"}},
						map[string]any{"name": "nic2", "multus": map[string]any{"networkName": templateNADPlaceholder + "/lan-nad"}},
					},
					"domain": map[string]any{"devices": map[string]any{"interfaces": []any{
						map[string]any{"name": "nic1", "bridge": map[string]any{}},
						map[string]any{"name": "nic2", "bridge": map[string]any{}},
					}}},
				},
			},
		},
	}}
	vm.SetLabels(map[string]string{LabelVMName: "router"})

	topo := &NetworkTopology{
		Subnets: []TopologySubnet{
			{Name: "mgmt", CIDR: "10.0.1.0/24"},
			{Name: "lan", CIDR: "192.168.0.0/24", Unmanaged: true},
		},
		VMNICs: map[string][]TopologyNIC{
			"router": {
				{Name: "nic1", Subnet: "mgmt", MAC: "52:54:00:00:00:01"},
				{Name: "nic2", Subnet: "lan", MAC: "52:54:00:00:00:02"},
			},
		},
	}

	if err := RewireVMNetworks(vm, topo, "ns-clone1", "ruddervirt-system"); err != nil {
		t.Fatalf("RewireVMNetworks: %v", err)
	}

	ifaces, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	byName := make(map[string]map[string]any, len(ifaces))
	for _, raw := range ifaces {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		byName[name] = m
	}

	if _, ok := byName["nic1"]["bridge"]; !ok {
		t.Error("managed nic1 should keep core bridge binding")
	}
	if _, ok := byName["nic1"]["binding"]; ok {
		t.Error("managed nic1 should not reference a binding plugin")
	}
	binding, ok := byName["nic2"]["binding"].(map[string]any)
	if !ok {
		t.Fatalf("unmanaged nic2 should reference a binding plugin, got %#v", byName["nic2"])
	}
	if binding["name"] != kubevirt.UnmanagedBindingName {
		t.Errorf("nic2 binding name = %v, want %s", binding["name"], kubevirt.UnmanagedBindingName)
	}
	if _, ok := byName["nic2"]["bridge"]; ok {
		t.Error("unmanaged nic2 should no longer have a bridge binding")
	}

	annots, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "annotations")
	mgmtRoutes := "ns-clone1-mgmt-subnet-nad.ruddervirt-system.ovn.kubernetes.io/routes"
	lanRoutes := "ns-clone1-lan-subnet-nad.ruddervirt-system.ovn.kubernetes.io/routes"
	if _, ok := annots[mgmtRoutes]; !ok {
		t.Errorf("managed subnet should have routes annotation %q", mgmtRoutes)
	}
	if _, ok := annots[lanRoutes]; ok {
		t.Errorf("unmanaged subnet should NOT have routes annotation %q", lanRoutes)
	}
}
