package namespace

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/nrednav/cuid2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// LabelOwnerKind identifies the CRD kind that owns the namespace.
	LabelOwnerKind = "ruddervirt.io/owner-kind"
	// LabelOwnerName identifies the CR name that owns the namespace.
	LabelOwnerName = "ruddervirt.io/owner-name"
	// LabelOwnerNamespace identifies the CR namespace that owns the namespace.
	LabelOwnerNamespace = "ruddervirt.io/owner-namespace"
)

// cuid2Gen generates CUID2 IDs of length 15.
var cuid2Gen func() string

func init() {
	gen, err := cuid2.Init(cuid2.WithLength(15))
	if err != nil {
		panic(fmt.Sprintf("initializing cuid2 generator: %v", err))
	}
	cuid2Gen = gen
}

// GenerateNamespaceName creates a namespace name from a prefix and a CUID2 suffix.
// Format: {prefix}{cuid2} (e.g. "vm-k8a3f9bx2c1mp4q" or "ns-j7d2e5hn9w3yk8r").
func GenerateNamespaceName(prefix string) string {
	return prefix + cuid2Gen()
}

// GenerateBuildID creates a unique build ID using CUID2.
// Format: "vm-{cuid2}" — used as the VirtualMachineNamespace name and resource prefix.
func GenerateBuildID() string {
	return "vm-" + cuid2Gen()
}

// FindOwnedNamespace searches for an existing namespace owned by the given CR.
// Returns the namespace name if found, or empty string if not found.
// This handles the race where a namespace was created but BuildNamespace wasn't persisted.
func FindOwnedNamespace(ctx context.Context, c client.Client, ownerName, ownerNamespace string) (string, error) {
	nsList := &corev1.NamespaceList{}
	if err := c.List(ctx, nsList, client.MatchingLabels{
		LabelOwnerName:      ownerName,
		LabelOwnerNamespace: ownerNamespace,
	}); err != nil {
		return "", fmt.Errorf("listing owned namespaces: %w", err)
	}
	for _, ns := range nsList.Items {
		if ns.DeletionTimestamp == nil {
			return ns.Name, nil
		}
	}
	return "", nil
}

// EnsureNamespace creates a child namespace with ownership labels if it doesn't exist.
// Returns true if the namespace was created (vs already existed).
// Optional extraLabels are merged into the namespace labels.
func EnsureNamespace(ctx context.Context, c client.Client, name, ownerKind, ownerName, ownerNamespace string, extraLabels ...map[string]string) (bool, error) {
	logger := log.FromContext(ctx)

	existing := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: name}, existing)
	if err == nil {
		return false, nil
	}
	if !errors.IsNotFound(err) {
		return false, fmt.Errorf("checking namespace %s: %w", name, err)
	}

	labels := map[string]string{
		LabelOwnerKind:      ownerKind,
		LabelOwnerName:      ownerName,
		LabelOwnerNamespace: ownerNamespace,
	}
	for _, extra := range extraLabels {
		maps.Copy(labels, extra)
	}

	logger.Info("Creating child namespace", "namespace", name, "owner", ownerKind+"/"+ownerName)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}

	if err := c.Create(ctx, ns); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating namespace %s: %w", name, err)
	}
	return true, nil
}

// ReplicateImagePullSecrets copies image pull secrets from the source namespace
// to the target namespace. Secret names are read from the IMAGE_PULL_SECRETS
// env var (comma-separated). Existing secrets are skipped.
func ReplicateImagePullSecrets(ctx context.Context, c client.Client, sourceNS, targetNS string) error {
	names := os.Getenv("IMAGE_PULL_SECRETS")
	if names == "" {
		return nil
	}
	logger := log.FromContext(ctx)
	for name := range strings.SplitSeq(names, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		// Check if it already exists in the target namespace.
		existing := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNS}, existing); err == nil {
			continue
		}
		// Read from source namespace.
		src := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: sourceNS}, src); err != nil {
			return fmt.Errorf("reading pull secret %s from %s: %w", name, sourceNS, err)
		}
		dst := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: targetNS,
			},
			Type: src.Type,
			Data: src.Data,
		}
		logger.Info("Replicating image pull secret", "secret", name, "to", targetNS)
		if err := c.Create(ctx, dst); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating pull secret %s in %s: %w", name, targetNS, err)
		}
	}
	return nil
}

// DeleteNamespace deletes a child namespace.
// Refuses to delete the operator's own namespace as a safety guard.
func DeleteNamespace(ctx context.Context, c client.Client, name string) error {
	logger := log.FromContext(ctx)

	operatorNS := os.Getenv("OPERATOR_NAMESPACE")
	if name == operatorNS || name == "kube-system" || name == "default" {
		logger.Error(nil, "BUG: refusing to delete protected namespace", "namespace", name)
		return fmt.Errorf("refusing to delete protected namespace %s", name)
	}

	logger.Info("Deleting child namespace", "namespace", name)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	if err := c.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", name, err)
	}
	return nil
}
