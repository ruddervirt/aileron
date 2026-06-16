package network

import (
	"context"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultExternalSubnet = "egress-external"
)

// ExternalSubnetName returns the KubeOVN external subnet used for egress.
// Reads from EGRESS_EXTERNAL_SUBNET env var, defaults to "egress-external".
func ExternalSubnetName() string {
	if s := os.Getenv("EGRESS_EXTERNAL_SUBNET"); s != "" {
		return s
	}
	return defaultExternalSubnet
}

var vpcEgressGatewayGVK = schema.GroupVersionKind{
	Group: "kubeovn.io", Version: "v1", Kind: "VpcEgressGateway",
}

// EnsureEgressGateway creates a VpcEgressGateway for a VPC so that VMs can
// reach the internet. The gateway performs SNAT from all internal subnets to the
// external subnet (e.g. egress-external → br-egress → masquerade → internet).
func EnsureEgressGateway(ctx context.Context, c client.Client, name, namespace, vpcName, internalSubnet string, allSubnets []string, labels map[string]string) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(vpcEgressGatewayGVK)
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)
	if err == nil {
		return nil // Already exists.
	}
	if !errors.IsNotFound(err) {
		return err
	}

	labelsIface := make(map[string]any, len(labels))
	for k, v := range labels {
		labelsIface[k] = v
	}

	subnetNames := make([]any, len(allSubnets))
	for i, s := range allSubnets {
		subnetNames[i] = s
	}

	gw := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeovn.io/v1",
			"kind":       "VpcEgressGateway",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"labels":    labelsIface,
			},
			"spec": map[string]any{
				"vpc":            vpcName,
				"replicas":       int64(1),
				"externalSubnet": ExternalSubnetName(),
				"internalSubnet": internalSubnet,
				"policies": []any{
					map[string]any{
						"snat": true,
						"ipBlocks": []any{
							"0.0.0.0/0",
						},
						"subnets": subnetNames,
					},
				},
			},
		},
	}

	return c.Create(ctx, gw)
}

// IsEgressGatewayReady checks whether the VpcEgressGateway has at least one
// ready replica and returns the internal IP for the VPC default route.
func IsEgressGatewayReady(ctx context.Context, c client.Client, name, namespace string) (bool, string, error) {
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(vpcEgressGatewayGVK)
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, gw)
	if err != nil {
		return false, "", err
	}

	// Check conditions for Ready.
	conditions, _, _ := unstructured.NestedSlice(gw.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		condStatus, _ := cond["status"].(string)
		if condType == "Ready" && condStatus == "True" {
			// Get internal IP for VPC route.
			internalIPs, _, _ := unstructured.NestedStringSlice(gw.Object, "status", "internalIPs")
			if len(internalIPs) > 0 {
				return true, internalIPs[0], nil
			}
			return true, "", nil
		}
	}

	// Fallback: check readyReplicas.
	readyReplicas, found, _ := unstructured.NestedInt64(gw.Object, "status", "readyReplicas")
	if found && readyReplicas > 0 {
		internalIPs, _, _ := unstructured.NestedStringSlice(gw.Object, "status", "internalIPs")
		if len(internalIPs) > 0 {
			return true, internalIPs[0], nil
		}
	}

	return false, "", nil
}

// EnsureVPCDefaultRoute adds a 0.0.0.0/0 static route to the VPC pointing at
// the egress gateway's internal IP.
func EnsureVPCDefaultRoute(ctx context.Context, c client.Client, vpcName, nextHopIP string) error {
	vpc := &unstructured.Unstructured{}
	vpc.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "Vpc",
	})
	if err := c.Get(ctx, types.NamespacedName{Name: vpcName}, vpc); err != nil {
		return fmt.Errorf("getting VPC %s for route: %w", vpcName, err)
	}

	staticRoutes, _, _ := unstructured.NestedSlice(vpc.Object, "spec", "staticRoutes")

	// Check if the route already exists.
	for _, r := range staticRoutes {
		route, ok := r.(map[string]any)
		if !ok {
			continue
		}
		cidr, _ := route["cidr"].(string)
		hop, _ := route["nextHopIP"].(string)
		if cidr == "0.0.0.0/0" && hop == nextHopIP {
			return nil // Already configured.
		}
	}

	// Remove any stale default route.
	filtered := make([]any, 0, len(staticRoutes))
	for _, r := range staticRoutes {
		route, ok := r.(map[string]any)
		if !ok {
			continue
		}
		cidr, _ := route["cidr"].(string)
		if cidr != "0.0.0.0/0" {
			filtered = append(filtered, r)
		}
	}

	// Add the new default route.
	filtered = append(filtered, map[string]any{
		"cidr":      "0.0.0.0/0",
		"nextHopIP": nextHopIP,
		"policy":    "policyDst",
	})

	if err := unstructured.SetNestedSlice(vpc.Object, filtered, "spec", "staticRoutes"); err != nil {
		return fmt.Errorf("setting static routes: %w", err)
	}

	if err := c.Update(ctx, vpc); err != nil {
		return fmt.Errorf("updating VPC %s with default route: %w", vpcName, err)
	}
	return nil
}

// DeleteEgressGateway deletes a VpcEgressGateway by name.
func DeleteEgressGateway(ctx context.Context, c client.Client, name, namespace string) error {
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(vpcEgressGatewayGVK)
	gw.SetName(name)
	gw.SetNamespace(namespace)

	if err := c.Delete(ctx, gw); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting VpcEgressGateway %s: %w", name, err)
	}
	return nil
}

// ScaleEgressGateway sets the replica count on a VpcEgressGateway.
// Use replicas=0 to stop the gateway, replicas=1 to start it.
func ScaleEgressGateway(ctx context.Context, c client.Client, name, namespace string, replicas int64) error {
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(vpcEgressGatewayGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, gw); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting VpcEgressGateway %s: %w", name, err)
	}

	current, _, _ := unstructured.NestedInt64(gw.Object, "spec", "replicas")
	if current == replicas {
		return nil
	}

	if err := unstructured.SetNestedField(gw.Object, replicas, "spec", "replicas"); err != nil {
		return fmt.Errorf("setting replicas on VpcEgressGateway %s: %w", name, err)
	}
	if err := c.Update(ctx, gw); err != nil {
		return fmt.Errorf("updating VpcEgressGateway %s: %w", name, err)
	}
	return nil
}

// IsEgressGatewayScaledDown checks if a VpcEgressGateway has 0 replicas.
func IsEgressGatewayScaledDown(ctx context.Context, c client.Client, name, namespace string) (bool, error) {
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(vpcEgressGatewayGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, gw); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	replicas, _, _ := unstructured.NestedInt64(gw.Object, "spec", "replicas")
	return replicas == 0, nil
}
