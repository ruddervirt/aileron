package network

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func kubeovnSubnet(name, cidr string) *unstructured.Unstructured {
	s := &unstructured.Unstructured{}
	s.SetGroupVersionKind(subnetGVK)
	s.SetName(name)
	_ = unstructured.SetNestedField(s.Object, cidr, "spec", "cidrBlock")
	return s
}

// TestCIDRInUse guards RemoveIPSetEntryIfUnused's sharing check: clones of the
// same template all declare the same CIDRs, and the ovn40subnets ipset holds a
// single entry per CIDR — it must only be removed once the LAST subnet
// declaring it is gone.
func TestCIDRInUse(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(subnetScheme()).WithObjects(
		kubeovnSubnet("ns-aaa-lan-subnet", "192.168.1.0/24"),
		kubeovnSubnet("ns-bbb-lan-subnet", "192.168.1.0/24"),
		kubeovnSubnet("ns-aaa-uplink-subnet", "10.0.1.0/24"),
	).Build()

	// Shared CIDR: still declared by remaining subnets.
	user, err := cidrInUse(context.Background(), c, "192.168.1.0/24")
	if err != nil {
		t.Fatalf("cidrInUse: %v", err)
	}
	if user == "" {
		t.Errorf("cidrInUse(192.168.1.0/24) = none, want a remaining subnet")
	}

	// CIDR nobody declares anymore: removable.
	user, err = cidrInUse(context.Background(), c, "10.9.9.0/24")
	if err != nil {
		t.Fatalf("cidrInUse: %v", err)
	}
	if user != "" {
		t.Errorf("cidrInUse(10.9.9.0/24) = %q, want none", user)
	}
}
