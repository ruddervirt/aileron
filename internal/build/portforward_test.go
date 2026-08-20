package build

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// vmiWithInterface builds a VMI whose named interface reports the given
// singular ipAddress and/or ipAddresses list, matching the shapes KubeVirt
// actually reports (both fields observed simultaneously on a live VMI, and
// older/partial statuses that only set ipAddress).
func vmiWithInterface(name, namespace, ifaceName, ipAddress string, ipAddresses []string) *unstructured.Unstructured {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	vmi.SetName(name)
	vmi.SetNamespace(namespace)

	iface := map[string]any{"name": ifaceName}
	if ipAddress != "" {
		iface["ipAddress"] = ipAddress
	}
	if len(ipAddresses) > 0 {
		addrs := make([]any, len(ipAddresses))
		for i, a := range ipAddresses {
			addrs[i] = a
		}
		iface["ipAddresses"] = addrs
	}
	_ = unstructured.SetNestedSlice(vmi.Object, []any{iface}, "status", "interfaces")
	return vmi
}

func TestGetVMIPFromVMI_PrefersIPv4OverLinkLocalIPv6(t *testing.T) {
	// Matches base-debian-graphical-yglkoyj8-cwcrz: right after a reboot,
	// KubeVirt reported the link-local address as the singular "primary"
	// ipAddress, but the ipAddresses list already carried the real IPv4
	// address too. GetVMIPFromVMI must prefer that IPv4 address.
	vmi := vmiWithInterface("debian", "vm-test", "eth0", "fe80::5054:ff:fe4d:be25",
		[]string{"10.0.1.4", "fe80::5054:ff:fe4d:be25"})
	cl := fake.NewClientBuilder().WithScheme(vmiScheme(t)).WithObjects(vmi).Build()

	ip, err := GetVMIPFromVMI(context.Background(), cl, "debian", "vm-test", "eth0")
	if err != nil {
		t.Fatalf("GetVMIPFromVMI: %v", err)
	}
	if ip != "10.0.1.4" {
		t.Errorf("ip = %q, want 10.0.1.4 (must not select the link-local address)", ip)
	}
}

func TestGetVMIPFromVMI_LinkLocalOnly_ReportsNotFound(t *testing.T) {
	// The transient post-reboot/post-boot window before DHCP has re-acquired
	// an IPv4 lease: only the auto-assigned link-local address is reported
	// yet. This must be treated as "no usable IP" so the caller's poll loop
	// keeps retrying instead of latching onto it.
	vmi := vmiWithInterface("debian", "vm-test", "eth0", "fe80::5054:ff:fe4d:be25",
		[]string{"fe80::5054:ff:fe4d:be25"})
	cl := fake.NewClientBuilder().WithScheme(vmiScheme(t)).WithObjects(vmi).Build()

	_, err := GetVMIPFromVMI(context.Background(), cl, "debian", "vm-test", "eth0")
	if err == nil {
		t.Fatal("GetVMIPFromVMI: want error for a link-local-only interface, got nil")
	}
}

func TestGetVMIPFromVMI_FallsBackToSingularIPAddress(t *testing.T) {
	// Guards the backward-compat path for a VMI status that only ever sets
	// the singular ipAddress field, without an ipAddresses list.
	vmi := vmiWithInterface("debian", "vm-test", "eth0", "10.0.1.4", nil)
	cl := fake.NewClientBuilder().WithScheme(vmiScheme(t)).WithObjects(vmi).Build()

	ip, err := GetVMIPFromVMI(context.Background(), cl, "debian", "vm-test", "eth0")
	if err != nil {
		t.Fatalf("GetVMIPFromVMI: %v", err)
	}
	if ip != "10.0.1.4" {
		t.Errorf("ip = %q, want 10.0.1.4", ip)
	}
}

func TestFirstUsableIP(t *testing.T) {
	cases := []struct {
		name  string
		addrs []string
		want  string
	}{
		{"ipv4 preferred over link-local ipv6", []string{"fe80::1", "10.0.1.4"}, "10.0.1.4"},
		{"link-local ipv6 only", []string{"fe80::1"}, ""},
		{"ipv4 preferred over routable ipv6", []string{"2001:db8::1", "10.0.1.4"}, "10.0.1.4"},
		{"routable ipv6 only", []string{"2001:db8::1"}, "2001:db8::1"},
		{"garbage skipped", []string{"not-an-ip", "10.0.1.4"}, "10.0.1.4"},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstUsableIP(c.addrs); got != c.want {
				t.Errorf("firstUsableIP(%v) = %q, want %q", c.addrs, got, c.want)
			}
		})
	}
}
