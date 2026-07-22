package network

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func egressScheme() *runtime.Scheme {
	s := vpcScheme()
	for _, gvk := range []struct{ kind string }{{"VpcEgressGateway"}} {
		g := vpcEgressGatewayGVK
		g.Kind = gvk.kind
		s.AddKnownTypeWithName(g, &unstructured.Unstructured{})
		lg := g
		lg.Kind += "List"
		s.AddKnownTypeWithName(lg, &unstructured.UnstructuredList{})
	}
	return s
}

// TestEnsureVPCDefaultRoute_ReplacesStaleRoute is the regression guard for a
// gateway pod replacement (crash, eviction, node drain) leaving the VPC's
// default route pinned to the old, now-dead pod IP forever. Any caller that
// re-invokes this with the gateway's current internal IP must correct it.
func TestEnsureVPCDefaultRoute_ReplacesStaleRoute(t *testing.T) {
	vpc := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeovn.io/v1",
			"kind":       "Vpc",
			"metadata":   map[string]any{"name": "vpc1"},
			"spec": map[string]any{
				"staticRoutes": []any{
					map[string]any{
						"cidr":      "0.0.0.0/0",
						"nextHopIP": "10.0.1.2", // stale: the replaced pod's old IP
						"policy":    "policyDst",
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(vpcScheme()).WithObjects(vpc).Build()

	if err := EnsureVPCDefaultRoute(context.Background(), c, "vpc1", "10.0.1.3"); err != nil {
		t.Fatalf("EnsureVPCDefaultRoute: %v", err)
	}

	got := getVPC(t, c, "vpc1")
	routes, _, _ := unstructured.NestedSlice(got.Object, "spec", "staticRoutes")
	if len(routes) != 1 {
		t.Fatalf("staticRoutes = %v, want exactly 1 route", routes)
	}
	route, _ := routes[0].(map[string]any)
	if hop, _ := route["nextHopIP"].(string); hop != "10.0.1.3" {
		t.Errorf("nextHopIP = %q, want %q (stale route was not corrected)", hop, "10.0.1.3")
	}
}

func newEgressGateway(name string, conditions []any, internalIPs []any, readyReplicas int64) *unstructured.Unstructured {
	status := map[string]any{}
	if conditions != nil {
		status["conditions"] = conditions
	}
	if internalIPs != nil {
		status["internalIPs"] = internalIPs
	}
	if readyReplicas > 0 {
		status["readyReplicas"] = readyReplicas
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeovn.io/v1",
			"kind":       "VpcEgressGateway",
			"metadata":   map[string]any{"name": name, "namespace": testNamespace},
			"spec":       map[string]any{},
			"status":     status,
		},
	}
}

func TestIsEgressGatewayReady_ReadyCondition(t *testing.T) {
	gw := newEgressGateway("gw1",
		[]any{map[string]any{"type": "Ready", "status": "True"}},
		[]any{"10.0.1.3"}, 0)
	c := fake.NewClientBuilder().WithScheme(egressScheme()).WithObjects(gw).Build()

	ready, ip, err := IsEgressGatewayReady(context.Background(), c, "gw1", testNamespace)
	if err != nil {
		t.Fatalf("IsEgressGatewayReady: %v", err)
	}
	if !ready || ip != "10.0.1.3" {
		t.Errorf("ready=%v ip=%q, want ready=true ip=10.0.1.3", ready, ip)
	}
}

// TestIsEgressGatewayReady_ReadyReplicasFallback guards the path that flagged
// the stale-route bug: readyReplicas can report ready before the Ready
// condition/internalIPs have settled to their final value. Callers must
// still get whatever internalIPs currently holds - if that's stale, the
// caller (EnsureVPCDefaultRoute) needs to be re-invoked later to correct it,
// which is why the fix re-verifies on every power-management poll rather
// than once.
func TestIsEgressGatewayReady_ReadyReplicasFallback(t *testing.T) {
	gw := newEgressGateway("gw1", nil, []any{"10.0.1.3"}, 1)
	c := fake.NewClientBuilder().WithScheme(egressScheme()).WithObjects(gw).Build()

	ready, ip, err := IsEgressGatewayReady(context.Background(), c, "gw1", testNamespace)
	if err != nil {
		t.Fatalf("IsEgressGatewayReady: %v", err)
	}
	if !ready || ip != "10.0.1.3" {
		t.Errorf("ready=%v ip=%q, want ready=true ip=10.0.1.3", ready, ip)
	}
}

func TestIsEgressGatewayReady_NotReady(t *testing.T) {
	gw := newEgressGateway("gw1", nil, nil, 0)
	c := fake.NewClientBuilder().WithScheme(egressScheme()).WithObjects(gw).Build()

	ready, _, err := IsEgressGatewayReady(context.Background(), c, "gw1", testNamespace)
	if err != nil {
		t.Fatalf("IsEgressGatewayReady: %v", err)
	}
	if ready {
		t.Errorf("ready = true, want false")
	}
}
