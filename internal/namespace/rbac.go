package namespace

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureRBAC is a no-op. The controller's ClusterRoleBinding already grants
// cluster-wide access via its ClusterRole, so no per-namespace RoleBinding
// is needed in child namespaces.
func EnsureRBAC(_ context.Context, _ client.Client, _, _, _ string) error {
	return nil
}
