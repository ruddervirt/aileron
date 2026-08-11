package clone

import (
	"context"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func validateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	return s
}

// TestFindBuild_ScopedToCloneNamespace guards against a regression where
// findBuild searched for a VirtualMachineBuild by name across every
// namespace in the cluster. Since the operator watches VirtualMachineBuild
// cluster-wide with no admission webhook restricting where it's created,
// two unrelated builds with the same .metadata.name in different
// namespaces used to be ambiguous — a clone could silently resolve to the
// wrong tenant's template. findBuild must only consider builds in the
// clone's own namespace.
func TestFindBuild_ScopedToCloneNamespace(t *testing.T) {
	wrongTenant := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-base", Namespace: "team-b"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "wrong-build-id"},
	}
	rightTenant := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-base", Namespace: "team-a"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "right-build-id"},
	}

	cl := fake.NewClientBuilder().WithScheme(validateScheme(t)).WithObjects(wrongTenant, rightTenant).Build()

	got, err := findBuild(context.Background(), cl, "team-a", "ubuntu-base")
	if err != nil {
		t.Fatalf("findBuild: %v", err)
	}
	if got == nil {
		t.Fatal("findBuild returned nil, want the team-a build")
		return
	}
	if got.Status.BuildID != "right-build-id" {
		t.Errorf("findBuild resolved buildID %q, want %q (team-a's own build, not team-b's)", got.Status.BuildID, "right-build-id")
	}
}

// TestFindBuild_NoMatchInNamespace confirms a build in another namespace
// never resolves as a fallback when the clone's own namespace has no
// matching build.
func TestFindBuild_NoMatchInNamespace(t *testing.T) {
	wrongTenant := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-base", Namespace: "team-b"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "wrong-build-id"},
	}
	cl := fake.NewClientBuilder().WithScheme(validateScheme(t)).WithObjects(wrongTenant).Build()

	got, err := findBuild(context.Background(), cl, "team-a", "ubuntu-base")
	if err != nil {
		t.Fatalf("findBuild: %v", err)
	}
	if got != nil {
		t.Errorf("findBuild = %+v, want nil (no build named ubuntu-base in team-a)", got)
	}
}
