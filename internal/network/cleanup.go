package network

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeOVN GVKs used during build teardown. VPCs and Subnets are cluster-scoped;
// VpcEgressGateways are namespaced (see vpcEgressGatewayGVK in egress.go).
var (
	vpcGVK    = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "Vpc"}
	subnetGVK = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "Subnet"}
)

// listNamesByLabel lists objects of the given kind matching the label selector
// and returns their names. namespace is empty for cluster-scoped kinds.
func listNamesByLabel(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace string, sel map[string]string) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)
	opts := []client.ListOption{client.MatchingLabels(sel)}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := c.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].GetName())
	}
	return names, nil
}

// ListBuildSubnets returns the names of all KubeOVN Subnets carrying the given
// label selector (e.g. the build-id label). Subnets are cluster-scoped.
func ListBuildSubnets(ctx context.Context, c client.Client, sel map[string]string) ([]string, error) {
	return listNamesByLabel(ctx, c, subnetGVK, "", sel)
}

// ListBuildVPCs returns the names of all KubeOVN VPCs carrying the given label
// selector. VPCs are cluster-scoped.
func ListBuildVPCs(ctx context.Context, c client.Client, sel map[string]string) ([]string, error) {
	return listNamesByLabel(ctx, c, vpcGVK, "", sel)
}

// ListBuildEgressGateways returns the names of all VpcEgressGateways in the
// given namespace carrying the given label selector.
func ListBuildEgressGateways(ctx context.Context, c client.Client, namespace string, sel map[string]string) ([]string, error) {
	return listNamesByLabel(ctx, c, vpcEgressGatewayGVK, namespace, sel)
}

// forceCleanResource ensures a single object is deleted, force-removing
// finalizers if it is stuck terminating. namespace is empty for cluster-scoped
// kinds. KubeOVN sets finalizers on VPCs and Subnets and clears them once
// dependent resources are gone; stripping them here guarantees teardown
// converges instead of wedging the owner's own finalizer forever.
func forceCleanResource(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj)
	if errors.IsNotFound(err) {
		return nil // Gone.
	}
	if err != nil {
		return err
	}

	if obj.GetDeletionTimestamp() == nil {
		if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting %s %s: %w", gvk.Kind, name, err)
		}
	}

	// Re-get to observe the deletion timestamp the API server just stamped, so
	// we only strip finalizers from an object that is actually terminating.
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if obj.GetDeletionTimestamp() != nil && len(obj.GetFinalizers()) > 0 {
		obj.SetFinalizers(nil)
		if err := c.Update(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("removing finalizers from %s %s: %w", gvk.Kind, name, err)
		}
	}
	return nil
}

// TeardownNetwork deletes every KubeOVN VpcEgressGateway, Subnet, and VPC
// belonging to a build or clone and reports whether any still remain.
//
// Resources are discovered by the label selector (the authoritative source —
// every VPC, Subnet, and gateway is labeled at creation with the owning
// build-id or clone-id) unioned with the names recorded in the owner's status,
// so a lost or never-written status can no longer orphan them. Deletion runs in
// dependency order (egress gateways -> subnets -> orphaned IPs -> VPCs) and
// force-removes stuck finalizers, so repeated calls converge to all-deleted.
//
// It returns done=true only once a fresh label sweep finds nothing, which the
// caller should treat as the gate for removing the owner's own finalizer.
// idPrefix is used to match KubeOVN IP objects by name prefix (empty to skip).
func TeardownNetwork(ctx context.Context, c client.Client, idPrefix, namespace string, sel map[string]string, statusVPCs, statusSubnets []string) (bool, error) {
	// Egress gateways first: their gateway pods hold IPs on the internal subnet.
	gwSel, err := ListBuildEgressGateways(ctx, c, namespace, sel)
	if err != nil {
		return false, fmt.Errorf("listing egress gateways: %w", err)
	}
	for _, name := range union(gwSel, derive(statusVPCs, "-egress")) {
		if err := forceCleanResource(ctx, c, vpcEgressGatewayGVK, namespace, name); err != nil {
			return false, err
		}
	}

	// Subnets next, so KubeOVN can release the VPC's logical switches.
	subnetSel, err := ListBuildSubnets(ctx, c, sel)
	if err != nil {
		return false, fmt.Errorf("listing subnets: %w", err)
	}
	for _, name := range union(subnetSel, statusSubnets) {
		if err := forceCleanResource(ctx, c, subnetGVK, "", name); err != nil {
			return false, err
		}
	}

	// Drop any KubeOVN IP objects left behind by deleted subnets.
	if idPrefix != "" {
		if err := CleanupOrphanedIPs(ctx, c, idPrefix); err != nil {
			return false, fmt.Errorf("cleaning orphaned IPs: %w", err)
		}
	}

	// VPCs last, once their subnets are gone.
	vpcSel, err := ListBuildVPCs(ctx, c, sel)
	if err != nil {
		return false, fmt.Errorf("listing VPCs: %w", err)
	}
	for _, name := range union(vpcSel, statusVPCs) {
		if err := forceCleanResource(ctx, c, vpcGVK, "", name); err != nil {
			return false, err
		}
	}

	// Re-sweep by label: report done only when nothing remains.
	gwSel, err = ListBuildEgressGateways(ctx, c, namespace, sel)
	if err != nil {
		return false, err
	}
	subnetSel, err = ListBuildSubnets(ctx, c, sel)
	if err != nil {
		return false, err
	}
	vpcSel, err = ListBuildVPCs(ctx, c, sel)
	if err != nil {
		return false, err
	}
	return len(gwSel)+len(subnetSel)+len(vpcSel) == 0, nil
}

// union returns the unique members of a and b, preserving a's order first.
func union(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, vals := range [][]string{a, b} {
		for _, v := range vals {
			if v == "" {
				continue
			}
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// derive appends suffix to each non-empty element of in. It reconstructs the
// egress-gateway names that EnsureEgressGateway derives from VPC names, used as
// a fallback for builds predating the build-id label on gateways.
func derive(in []string, suffix string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		out = append(out, v+suffix)
	}
	return out
}
