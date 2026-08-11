package build

import (
	"context"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func relayScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	return s
}

// TestEnsureRelayPod_StaleCleanupScopedToBuild guards against a regression
// where the "stale relay pod" sweep in EnsureRelayPod matched by
// LabelBuild (build.Name) alone. Because all builds share the operator
// namespace, LabelBuild is not unique across tenant namespaces — two
// VirtualMachineBuild CRs in different namespaces can share the same
// .metadata.name — so an unscoped sweep could delete another build's
// still-in-use relay pod. LabelBuildID (globally unique) must be part of
// the selector instead.
func TestEnsureRelayPod_StaleCleanupScopedToBuild(t *testing.T) {
	otherBuildRelay := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-build-id-relay",
			Namespace: "ruddervirt-system",
			Labels: map[string]string{
				LabelBuildID:   "other-build-id",
				LabelBuild:     "shared-name",
				LabelComponent: "relay",
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(relayScheme(t)).WithObjects(otherBuildRelay).Build()
	mgr := &RelayPodManager{Client: cl}

	vmBuild := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-name", Namespace: "team-a"},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:        "this-build-id",
			BuildNamespace: "ruddervirt-system",
		},
	}

	if err := mgr.EnsureRelayPod(context.Background(), vmBuild); err != nil {
		t.Fatalf("EnsureRelayPod: %v", err)
	}

	got := &corev1.Pod{}
	err := cl.Get(context.Background(), types.NamespacedName{Name: "other-build-id-relay", Namespace: "ruddervirt-system"}, got)
	if err != nil {
		t.Fatalf("other build's relay pod should survive EnsureRelayPod, got error: %v", err)
	}

	own := &corev1.Pod{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "this-build-id-relay", Namespace: "ruddervirt-system"}, own); err != nil {
		t.Errorf("own relay pod should have been created, got error: %v", err)
	}
}
