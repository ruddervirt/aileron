package network

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var nadGVK = schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"}

func subnetScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{subnetGVK, nadGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		lg := gvk
		lg.Kind += "List"
		s.AddKnownTypeWithName(lg, &unstructured.UnstructuredList{})
	}
	return s
}

func createdSubnetDHCPOptions(t *testing.T, spec SubnetSpec, internet bool) string {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(subnetScheme()).Build()
	if err := EnsureSubnet(context.Background(), c, "s1", "vpc1", testNamespace, spec, internet, nil); err != nil {
		t.Fatalf("EnsureSubnet: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(subnetGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "s1"}, got); err != nil {
		t.Fatalf("get subnet: %v", err)
	}
	opts, _, _ := unstructured.NestedString(got.Object, "spec", "dhcpV4Options")
	return opts
}

// TestEnsureSubnet_MultiDNSUsesSemicolons is the regression guard for the WAN
// DHCP outage: KubeOVN parses dhcpV4Options as a comma-separated key=value
// list, so a multi-resolver dns_server must use ';' between IPs. A literal ','
// truncated the value to "{8.8.8.8" and OVN then served no DHCP offer at all.
func TestEnsureSubnet_MultiDNSUsesSemicolons(t *testing.T) {
	// internet=true -> SubnetDNS returns "8.8.8.8,1.1.1.1".
	opts := createdSubnetDHCPOptions(t, SubnetSpec{CIDR: "10.0.1.0/24", DHCP: true}, true)
	want := "dns_server={8.8.8.8;1.1.1.1}"
	if opts != want {
		t.Errorf("dhcpV4Options = %q, want %q", opts, want)
	}
}

func TestEnsureSubnet_ExplicitMultiDNSConverted(t *testing.T) {
	opts := createdSubnetDHCPOptions(t, SubnetSpec{CIDR: "10.0.1.0/24", DHCP: true, DNS: "9.9.9.9,1.0.0.1"}, false)
	want := "dns_server={9.9.9.9;1.0.0.1}"
	if opts != want {
		t.Errorf("dhcpV4Options = %q, want %q", opts, want)
	}
}

func TestEnsureSubnet_SingleDNSUnchanged(t *testing.T) {
	// internet=false, no explicit DNS -> SubnetDNS returns the gateway IP (no comma).
	opts := createdSubnetDHCPOptions(t, SubnetSpec{CIDR: "10.0.1.0/24", DHCP: true}, false)
	want := "dns_server={10.0.1.1}"
	if opts != want {
		t.Errorf("dhcpV4Options = %q, want %q", opts, want)
	}
}

// createdSubnetSpec runs EnsureSubnet against a fake client and returns the
// created KubeOVN Subnet's spec map for field-level assertions.
func createdSubnetSpec(t *testing.T, spec SubnetSpec, internet bool) map[string]any {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(subnetScheme()).Build()
	if err := EnsureSubnet(context.Background(), c, "s1", "vpc1", testNamespace, spec, internet, nil); err != nil {
		t.Fatalf("EnsureSubnet: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(subnetGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "s1"}, got); err != nil {
		t.Fatalf("get subnet: %v", err)
	}
	m, _, _ := unstructured.NestedMap(got.Object, "spec")
	return m
}

// TestEnsureSubnet_UnmanagedRelocatesGateway is the regression guard for the
// gateway-squat outage: KubeOVN attaches a gateway router port to every
// overlay subnet at spec.gateway (logicalGateway only applies to underlay),
// and at the default first-usable IP it shadowed pfSense's 192.168.1.1 —
// clients leased from pfSense but every packet to .1 hit OVN instead.
// ApplyUnmanaged must park OVN's gateway on the second-to-last usable IP and
// reserve the first usable for the guest.
func TestEnsureSubnet_UnmanagedRelocatesGateway(t *testing.T) {
	spec := SubnetSpec{CIDR: "192.168.1.0/24", DHCP: true}
	if err := spec.ApplyUnmanaged(); err != nil {
		t.Fatalf("ApplyUnmanaged: %v", err)
	}
	m := createdSubnetSpec(t, spec, false)

	if gw := m["gateway"]; gw != "192.168.1.253" {
		t.Errorf("gateway = %v, want 192.168.1.253", gw)
	}
	if dhcp := m["enableDHCP"]; dhcp != false {
		t.Errorf("enableDHCP = %v, want false", dhcp)
	}
	if dgc := m["disableGatewayCheck"]; dgc != true {
		t.Errorf("disableGatewayCheck = %v, want true", dgc)
	}
	excl, ok := m["excludeIps"].([]any)
	if !ok || len(excl) != 1 || excl[0] != "192.168.1.1" {
		t.Errorf("excludeIps = %v, want [192.168.1.1]", m["excludeIps"])
	}
	if opts, present := m["dhcpV4Options"]; present {
		t.Errorf("dhcpV4Options = %v, want unset (OVN DHCP is off)", opts)
	}
}

func TestEnsureSubnet_ManagedKeepsDefaultGateway(t *testing.T) {
	m := createdSubnetSpec(t, SubnetSpec{CIDR: "10.0.1.0/24", DHCP: true}, false)
	if gw, present := m["gateway"]; present {
		t.Errorf("gateway = %v, want unset (KubeOVN default first-usable)", gw)
	}
}

func TestApplyUnmanaged(t *testing.T) {
	tests := []struct {
		cidr        string
		wantGateway string
		wantExclude string
		wantErr     bool
	}{
		{cidr: "192.168.1.0/24", wantGateway: "192.168.1.253", wantExclude: "192.168.1.1"},
		{cidr: "10.5.0.0/16", wantGateway: "10.5.255.253", wantExclude: "10.5.0.1"},
		// /29 is the narrowest allowed: usable .1-.6, OVN gateway at .5.
		{cidr: "10.0.1.0/29", wantGateway: "10.0.1.5", wantExclude: "10.0.1.1"},
		// /30 leaves the second-to-last usable == first usable: rejected.
		{cidr: "10.0.1.0/30", wantErr: true},
		{cidr: "not-a-cidr", wantErr: true},
	}
	for _, tt := range tests {
		spec := SubnetSpec{CIDR: tt.cidr, DHCP: true}
		err := spec.ApplyUnmanaged()
		if tt.wantErr {
			if err == nil {
				t.Errorf("ApplyUnmanaged(%q): want error, got nil", tt.cidr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ApplyUnmanaged(%q): %v", tt.cidr, err)
			continue
		}
		if spec.Gateway != tt.wantGateway {
			t.Errorf("ApplyUnmanaged(%q): Gateway = %q, want %q", tt.cidr, spec.Gateway, tt.wantGateway)
		}
		if len(spec.ExcludeIPs) != 1 || spec.ExcludeIPs[0] != tt.wantExclude {
			t.Errorf("ApplyUnmanaged(%q): ExcludeIPs = %v, want [%s]", tt.cidr, spec.ExcludeIPs, tt.wantExclude)
		}
		if spec.DHCP {
			t.Errorf("ApplyUnmanaged(%q): DHCP still true", tt.cidr)
		}
		if !spec.DisableGatewayCheck {
			t.Errorf("ApplyUnmanaged(%q): DisableGatewayCheck not set", tt.cidr)
		}
	}
}
