package build

import (
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/kubevirt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestVMAnnotationsForNICs(t *testing.T) {
	nics := []v1alpha1.VMNIC{
		{Name: "eth0", Subnet: "frontend"},
		{Name: "eth1", Subnet: "backend", IP: "10.0.2.10", MAC: "00:11:22:33:44:55"},
	}

	annots := VMAnnotationsForNICs("vm-abc123", nics)

	tests := map[string]string{
		"ruddervirtvirt.io/net.vm.nic.eth0.subnet": "vm-abc123-frontend-subnet",
		"ruddervirtvirt.io/net.vm.nic.eth1.subnet": "vm-abc123-backend-subnet",
		"ruddervirtvirt.io/net.vm.nic.eth1.ip":     "10.0.2.10",
		"ruddervirtvirt.io/net.vm.nic.eth1.mac":    "00:11:22:33:44:55",
	}

	for key, want := range tests {
		got, ok := annots[key]
		if !ok {
			t.Errorf("missing annotation %s", key)
			continue
		}
		if got != want {
			t.Errorf("annotation %s = %s, want %s", key, got, want)
		}
	}

	// eth0 should NOT have ip or mac.
	if _, ok := annots["ruddervirtvirt.io/net.vm.nic.eth0.ip"]; ok {
		t.Error("eth0 should not have ip annotation")
	}
	if _, ok := annots["ruddervirtvirt.io/net.vm.nic.eth0.mac"]; ok {
		t.Error("eth0 should not have mac annotation")
	}
}

// TestBuildVMBindingForUnmanagedSubnet asserts the per-NIC binding selection:
// unmanaged subnets use the managedTap binding plugin (no virt-launcher DHCP)
// and get no default-route annotation; managed subnets keep core bridge binding
// and the route annotation.
func TestBuildVMBindingForUnmanagedSubnet(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "fenceline", Namespace: "ruddervirt-system"},
		Spec: v1alpha1.VirtualMachineBuildSpec{
			Network: &v1alpha1.Network{
				VPCs: []v1alpha1.VPC{{Name: "vpc"}},
				Subnets: []v1alpha1.Subnet{
					{Name: "mgmt", VPC: "vpc", CIDR: "10.0.1.0/24"},
					{Name: "lan", VPC: "vpc", CIDR: "192.168.0.0/24", Unmanaged: true},
				},
			},
			VMs: []v1alpha1.BuildVM{{
				Name:   "router",
				Source: v1alpha1.BuildSource{Blank: true},
				NICs: []v1alpha1.VMNIC{
					{Name: "nic1", Subnet: "mgmt", MAC: "52:54:00:00:00:01"},
					{Name: "nic2", Subnet: "lan", MAC: "52:54:00:00:00:02"},
				},
			}},
		},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:        "vm-test1",
			BuildNamespace: "ruddervirt-system",
		},
	}

	vm, err := (&VMBooter{}).buildVM(build, &build.Spec.VMs[0])
	if err != nil {
		t.Fatalf("buildVM: %v", err)
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

	// Managed NIC -> core bridge binding, no plugin.
	if _, ok := byName["nic1"]["bridge"]; !ok {
		t.Error("managed nic1 should use core bridge binding")
	}
	if _, ok := byName["nic1"]["binding"]; ok {
		t.Error("managed nic1 should not reference a binding plugin")
	}

	// Unmanaged NIC -> managedTap binding plugin, no bridge.
	binding, ok := byName["nic2"]["binding"].(map[string]any)
	if !ok {
		t.Fatalf("unmanaged nic2 should reference a binding plugin, got %#v", byName["nic2"])
	}
	if binding["name"] != kubevirt.UnmanagedBindingName {
		t.Errorf("nic2 binding name = %v, want %s", binding["name"], kubevirt.UnmanagedBindingName)
	}
	if _, ok := byName["nic2"]["bridge"]; ok {
		t.Error("unmanaged nic2 should not have a bridge binding")
	}

	// Routes annotation present for managed, absent for unmanaged.
	annots, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "annotations")
	mgmtRoutes := buildSubnetName("vm-test1", "mgmt") + "-nad.ruddervirt-system.ovn.kubernetes.io/routes"
	lanRoutes := buildSubnetName("vm-test1", "lan") + "-nad.ruddervirt-system.ovn.kubernetes.io/routes"
	if _, ok := annots[mgmtRoutes]; !ok {
		t.Errorf("managed subnet should have routes annotation %q", mgmtRoutes)
	}
	if _, ok := annots[lanRoutes]; ok {
		t.Errorf("unmanaged subnet should NOT have routes annotation %q", lanRoutes)
	}
}

// TestBuildVMBindingUnmanagedOverriddenManaged proves the buildOverrides
// subnet escape hatch: a segment that is unmanaged in the base spec (so clones
// inherit it) but overridden managed for the build gets core bridge binding on
// the LIVE build VM (virt-launcher DHCP, so provisioning can reach it), while
// the routes annotation and l2bridge are reserved for the clone path.
func TestBuildVMBindingUnmanagedOverriddenManaged(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "fenceline", Namespace: "ruddervirt-system"},
		Spec: v1alpha1.VirtualMachineBuildSpec{
			Network: &v1alpha1.Network{
				VPCs: []v1alpha1.VPC{{Name: "vpc"}},
				Subnets: []v1alpha1.Subnet{
					{Name: "lan", VPC: "vpc", CIDR: "192.168.1.0/24", Unmanaged: true},
				},
			},
			VMs: []v1alpha1.BuildVM{{
				Name:   "workstation",
				Source: v1alpha1.BuildSource{Blank: true},
				NICs:   []v1alpha1.VMNIC{{Name: "nic1", Subnet: "lan", MAC: "52:54:00:00:00:01"}},
			}},
			BuildOverrides: &v1alpha1.BuildOverrides{
				Subnets: []v1alpha1.SubnetOverride{{Name: "lan", Unmanaged: new(false)}},
			},
		},
		Status: v1alpha1.VirtualMachineBuildStatus{BuildID: "vm-test1", BuildNamespace: "ruddervirt-system"},
	}

	vm, err := (&VMBooter{}).buildVM(build, &build.Spec.VMs[0])
	if err != nil {
		t.Fatalf("buildVM: %v", err)
	}
	ifaces, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	if len(ifaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(ifaces))
	}
	nic1 := ifaces[0].(map[string]any)
	if _, ok := nic1["bridge"]; !ok {
		t.Error("overridden-managed lan nic1 should use core bridge binding during build")
	}
	if _, ok := nic1["binding"]; ok {
		t.Error("overridden-managed lan nic1 should NOT use the l2bridge plugin during build")
	}

	// The base spec is still unmanaged, so the template topology must capture it
	// for clones (which is what makes the segment guest-served at runtime).
	if !subnetIsUnmanaged(build, "lan") {
		t.Error("base spec lan must remain unmanaged for the clone path")
	}
	if effectiveSubnetUnmanaged(build, "lan") {
		t.Error("lan must be effective-managed for the build under the override")
	}
}

func TestBuildVPCName(t *testing.T) {
	got := buildVPCName("vm-abc123", "public")
	if got != "vm-abc123-public-vpc" {
		t.Errorf("got %s, want vm-abc123-public-vpc", got)
	}
}

func TestBuildSubnetName(t *testing.T) {
	got := buildSubnetName("vm-abc123", "public")
	if got != "vm-abc123-public-subnet" {
		t.Errorf("got %s, want vm-abc123-public-subnet", got)
	}
}
