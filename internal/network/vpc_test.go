package network

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// listKindSuffix is the suffix Kubernetes list kinds use (e.g. Vpc -> VpcList).
const listKindSuffix = "List"

// listGVKFor returns the List variant of a GVK, for registering the
// corresponding UnstructuredList type on a fake client scheme.
func listGVKFor(gvk schema.GroupVersionKind) schema.GroupVersionKind {
	gvk.Kind += listKindSuffix
	return gvk
}

func vpcScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(vpcGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(listGVKFor(vpcGVK), &unstructured.UnstructuredList{})
	return s
}

func getVPC(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(vpcGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("get vpc %s: %v", name, err)
	}
	return got
}

// TestEnsureVPC_NeverEnablesExternal guards against re-wiring aileron's
// "internet" concept into KubeOVN's own EIP/SNAT enableExternal feature: that
// feature needs a subnet named "external" this cluster doesn't have, and
// enabling it puts the VPC into KubeOVN's EIP-SNAT reconcile loop forever.
// Aileron's real internet egress goes through VpcEgressGateway instead.
func TestEnsureVPC_NeverEnablesExternal(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(vpcScheme()).Build()
	if err := EnsureVPC(context.Background(), c, "vpc1", testNamespace, nil); err != nil {
		t.Fatalf("EnsureVPC: %v", err)
	}
	got := getVPC(t, c, "vpc1")
	external, _, _ := unstructured.NestedBool(got.Object, "spec", "enableExternal")
	if external {
		t.Errorf("spec.enableExternal = true, want false")
	}
}

// TestEnsureVPC_CorrectsDriftedExternal guards the migration path for VPCs
// created before this fix, which have enableExternal:true baked in.
func TestEnsureVPC_CorrectsDriftedExternal(t *testing.T) {
	existing := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeovn.io/v1",
			"kind":       "Vpc",
			"metadata":   map[string]any{"name": "vpc1"},
			"spec": map[string]any{
				"enableExternal": true,
				"namespaces":     []any{testNamespace},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(vpcScheme()).WithObjects(existing).Build()
	if err := EnsureVPC(context.Background(), c, "vpc1", testNamespace, nil); err != nil {
		t.Fatalf("EnsureVPC: %v", err)
	}
	got := getVPC(t, c, "vpc1")
	external, _, _ := unstructured.NestedBool(got.Object, "spec", "enableExternal")
	if external {
		t.Errorf("spec.enableExternal = true after drift correction, want false")
	}
}
