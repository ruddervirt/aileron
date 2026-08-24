// SPDX-License-Identifier: GPL-3.0-only

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

// EnsureVPC creates a KubeOVN VPC if it doesn't exist.
// The namespace parameter binds the VPC to the build namespace, which is
// required for KubeOVN to route traffic from pods in that namespace.
//
// enableExternal is deliberately never set to true here: it's KubeOVN's own
// built-in EIP/SNAT external-gateway feature, which requires a subnet named
// "external" that this cluster doesn't have and doesn't need — aileron's
// internet egress is provided independently via VpcEgressGateway (see
// EnsureEgressGateway) and subnet-level natOutgoing. Leaving enableExternal
// true puts the VPC into KubeOVN's EIP-SNAT reconcile loop forever.
func EnsureVPC(ctx context.Context, c client.Client, name, namespace string, labels map[string]string) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "Vpc",
	})
	err := c.Get(ctx, types.NamespacedName{Name: name}, existing)
	if err == nil {
		if existing.GetDeletionTimestamp() != nil {
			return fmt.Errorf("VPC %s is being deleted, waiting for cleanup", name)
		}
		// Correct any VPC left with enableExternal:true from before this fix.
		currentExternal, _, _ := unstructured.NestedBool(existing.Object, "spec", "enableExternal")
		if currentExternal {
			if err := unstructured.SetNestedField(existing.Object, false, "spec", "enableExternal"); err != nil {
				return fmt.Errorf("setting enableExternal on VPC %s: %w", name, err)
			}
			if err := c.Update(ctx, existing); err != nil {
				return fmt.Errorf("updating VPC %s enableExternal: %w", name, err)
			}
		}
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	labelsIface := make(map[string]any, len(labels))
	for k, v := range labels {
		labelsIface[k] = v
	}

	vpc := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeovn.io/v1",
			"kind":       "Vpc",
			"metadata": map[string]any{
				"name":   name,
				"labels": labelsIface,
			},
			"spec": map[string]any{
				"enableExternal": false,
				"namespaces":     []any{namespace},
			},
		},
	}

	return c.Create(ctx, vpc)
}

// IsVPCReady checks if a KubeOVN VPC has been assigned a router.
func IsVPCReady(ctx context.Context, c client.Client, name string) (bool, error) {
	vpc := &unstructured.Unstructured{}
	vpc.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "Vpc",
	})
	err := c.Get(ctx, types.NamespacedName{Name: name}, vpc)
	if err != nil {
		return false, err
	}

	router, _, _ := unstructured.NestedString(vpc.Object, "status", "router")
	return router != "", nil
}

// DeleteVPC deletes a KubeOVN VPC.
func DeleteVPC(ctx context.Context, c client.Client, name string) error {
	vpc := &unstructured.Unstructured{}
	vpc.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "Vpc",
	})
	vpc.SetName(name)

	if err := c.Delete(ctx, vpc); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting VPC %s: %w", name, err)
	}
	return nil
}
