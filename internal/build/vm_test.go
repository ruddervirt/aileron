package build

import (
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCPUCores(t *testing.T) {
	cases := []struct {
		cpu  string
		want int64
	}{
		{"0", 1},     // defensive floor
		{"100m", 1},  // fractional rounds up to 1
		{"0.1", 1},   // same, decimal form
		{"500m", 1},  // still 1 core
		{"1", 1},     // exact
		{"1.5", 2},   // rounds up
		{"2", 2},     // exact
		{"2.5", 3},   // rounds up
		{"4", 4},     // exact
		{"4000m", 4}, // milli form
	}
	for _, c := range cases {
		got := cpuCores(resource.MustParse(c.cpu))
		if got != c.want {
			t.Errorf("cpuCores(%q) = %d, want %d", c.cpu, got, c.want)
		}
	}
}

func buildForInvisibleTest(invisible bool) *v1alpha1.VirtualMachineBuild {
	return &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "invistest", Namespace: "ruddervirt-system"},
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{{
				Name:      "webserver",
				Source:    v1alpha1.BuildSource{Blank: true},
				Invisible: invisible,
			}},
		},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:        "vm-test1",
			BuildNamespace: "ruddervirt-system",
		},
	}
}

func TestBuildVMStampsInvisibleAnnotation(t *testing.T) {
	build := buildForInvisibleTest(true)
	vm, err := (&VMBooter{}).buildVM(build, &build.Spec.VMs[0])
	if err != nil {
		t.Fatalf("buildVM: %v", err)
	}
	annotations, _, _ := unstructured.NestedStringMap(vm.Object, "metadata", "annotations")
	if got := annotations[v1alpha1.AnnotationInvisible]; got != valueTrue {
		t.Errorf("metadata.annotations[%s] = %q, want %q", v1alpha1.AnnotationInvisible, got, valueTrue)
	}
}

func TestBuildVMOmitsInvisibleAnnotationWhenFalse(t *testing.T) {
	build := buildForInvisibleTest(false)
	vm, err := (&VMBooter{}).buildVM(build, &build.Spec.VMs[0])
	if err != nil {
		t.Fatalf("buildVM: %v", err)
	}
	annotations, _, _ := unstructured.NestedStringMap(vm.Object, "metadata", "annotations")
	if _, ok := annotations[v1alpha1.AnnotationInvisible]; ok {
		t.Errorf("metadata.annotations[%s] should be absent, got %q", v1alpha1.AnnotationInvisible, annotations[v1alpha1.AnnotationInvisible])
	}
}
