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

// kubeOVNScheme registers the cluster- and namespace-scoped KubeOVN kinds (and
// their List kinds) the teardown helpers touch, so the fake client can serve
// unstructured List/Get/Delete/Update for them.
func kubeOVNScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{vpcGVK, subnetGVK, vpcEgressGatewayGVK, ipGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		listGVK := gvk
		listGVK.Kind += "List"
		s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	return s
}

var ipGVK = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "IP"}

const testNamespace = "ruddervirt-system"

// kubeOVNObj builds an unstructured KubeOVN object of the given kind. namespace
// is empty for cluster-scoped kinds. finalizers, when set, exercise the
// force-clean path.
func kubeOVNObj(gvk schema.GroupVersionKind, namespace, name string, labels map[string]string, finalizers ...string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetName(name)
	if namespace != "" {
		o.SetNamespace(namespace)
	}
	o.SetLabels(labels)
	if len(finalizers) > 0 {
		o.SetFinalizers(finalizers)
	}
	return o
}

func exists(t *testing.T, c client.Client, gvk schema.GroupVersionKind, namespace, name string) bool {
	t.Helper()
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, o)
	return err == nil
}

func TestListBuildResources_FiltersByLabel(t *testing.T) {
	ns := testNamespace
	mine := map[string]string{"ruddervirt.io/build-id": "vm-mine"}
	theirs := map[string]string{"ruddervirt.io/build-id": "vm-other"}

	c := fake.NewClientBuilder().WithScheme(kubeOVNScheme()).WithObjects(
		kubeOVNObj(vpcGVK, "", "vm-mine-vpc", mine),
		kubeOVNObj(vpcGVK, "", "vm-other-vpc", theirs),
		kubeOVNObj(subnetGVK, "", "vm-mine-subnet", mine),
		kubeOVNObj(vpcEgressGatewayGVK, ns, "vm-mine-vpc-egress", mine),
		kubeOVNObj(vpcEgressGatewayGVK, ns, "vm-other-vpc-egress", theirs),
	).Build()

	ctx := context.Background()
	sel := mine

	vpcs, err := ListBuildVPCs(ctx, c, sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(vpcs) != 1 || vpcs[0] != "vm-mine-vpc" {
		t.Errorf("ListBuildVPCs = %v, want [vm-mine-vpc]", vpcs)
	}

	gws, err := ListBuildEgressGateways(ctx, c, ns, sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(gws) != 1 || gws[0] != "vm-mine-vpc-egress" {
		t.Errorf("ListBuildEgressGateways = %v, want [vm-mine-vpc-egress]", gws)
	}
}

// TestTeardownNetwork_DeletesEverything is the core regression test: a full set
// of labeled VPC/subnet/egress-gateway resources (the gateway holding a stuck
// finalizer) is torn down to completion, another build's resources are left
// untouched, and done flips to true only once nothing remains.
func TestTeardownNetwork_DeletesEverything(t *testing.T) {
	ns := testNamespace
	mine := map[string]string{"ruddervirt.io/build-id": "vm-mine"}
	theirs := map[string]string{"ruddervirt.io/build-id": "vm-other"}

	c := fake.NewClientBuilder().WithScheme(kubeOVNScheme()).WithObjects(
		kubeOVNObj(vpcGVK, "", "vm-mine-vpc", mine),
		kubeOVNObj(subnetGVK, "", "vm-mine-subnet", mine, "kubeovn.io/subnet-finalizer"),
		kubeOVNObj(vpcEgressGatewayGVK, ns, "vm-mine-vpc-egress", mine),
		// A different build's resources must survive.
		kubeOVNObj(vpcGVK, "", "vm-other-vpc", theirs),
	).Build()

	ctx := context.Background()
	done, err := TeardownNetwork(ctx, c, "vm-mine", ns, mine, nil, nil)
	if err != nil {
		t.Fatalf("TeardownNetwork: %v", err)
	}
	if !done {
		t.Fatalf("done = false, want true after teardown of all resources")
	}

	if exists(t, c, vpcGVK, "", "vm-mine-vpc") {
		t.Error("VPC vm-mine-vpc still exists")
	}
	if exists(t, c, subnetGVK, "", "vm-mine-subnet") {
		t.Error("subnet vm-mine-subnet still exists (finalizer not force-removed)")
	}
	if exists(t, c, vpcEgressGatewayGVK, ns, "vm-mine-vpc-egress") {
		t.Error("egress gateway vm-mine-vpc-egress still exists")
	}
	if !exists(t, c, vpcGVK, "", "vm-other-vpc") {
		t.Error("VPC vm-other-vpc was deleted but belongs to a different build")
	}
}

// TestTeardownNetwork_StatusFallback covers the original orphaning bug: a
// resource that lost (or never got) its label is still discovered via the
// status name lists and deleted.
func TestTeardownNetwork_StatusFallback(t *testing.T) {
	ns := testNamespace
	sel := map[string]string{"ruddervirt.io/build-id": "vm-mine"}

	// Unlabeled resources — a label sweep alone would miss these.
	c := fake.NewClientBuilder().WithScheme(kubeOVNScheme()).WithObjects(
		kubeOVNObj(vpcGVK, "", "vm-mine-vpc", nil),
		kubeOVNObj(subnetGVK, "", "vm-mine-subnet", nil),
		kubeOVNObj(vpcEgressGatewayGVK, ns, "vm-mine-vpc-egress", nil),
	).Build()

	ctx := context.Background()
	done, err := TeardownNetwork(ctx, c, "vm-mine", ns,
		sel,
		[]string{"vm-mine-vpc"},    // statusVPCs (also yields vm-mine-vpc-egress via derive)
		[]string{"vm-mine-subnet"}, // statusSubnets
	)
	if err != nil {
		t.Fatalf("TeardownNetwork: %v", err)
	}
	if !done {
		t.Fatalf("done = false, want true")
	}
	if exists(t, c, vpcGVK, "", "vm-mine-vpc") {
		t.Error("status-listed VPC was not deleted")
	}
	if exists(t, c, subnetGVK, "", "vm-mine-subnet") {
		t.Error("status-listed subnet was not deleted")
	}
	if exists(t, c, vpcEgressGatewayGVK, ns, "vm-mine-vpc-egress") {
		t.Error("derived egress gateway was not deleted")
	}
}

func TestUnion(t *testing.T) {
	got := union([]string{"a", "b", ""}, []string{"b", "c", ""})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("union = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("union = %v, want %v", got, want)
		}
	}
}

func TestDerive(t *testing.T) {
	got := derive([]string{"vm-a-vpc", "", "vm-b-vpc"}, "-egress")
	want := []string{"vm-a-vpc-egress", "vm-b-vpc-egress"}
	if len(got) != len(want) {
		t.Fatalf("derive = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("derive = %v, want %v", got, want)
		}
	}
}
