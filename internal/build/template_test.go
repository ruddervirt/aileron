package build

import (
	"context"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// volumeByName returns the volume map with the given name, or nil.
func volumeByName(t *testing.T, vm *unstructured.Unstructured, name string) map[string]any {
	t.Helper()
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for _, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if n, _, _ := unstructured.NestedString(vol, "name"); n == name {
			return vol
		}
	}
	return nil
}

func claimName(t *testing.T, vol map[string]any) string {
	t.Helper()
	if vol == nil {
		return ""
	}
	pvc, ok := vol["persistentVolumeClaim"].(map[string]any)
	if !ok {
		return ""
	}
	name, _, _ := unstructured.NestedString(pvc, "claimName")
	return name
}

func templateVMWithVolumes(volumes []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"volumes": volumes,
					},
				},
			},
		},
	}
}

// TestRebuildVolumes_MultiDisk guards the multi-disk regression: rebuildVolumes
// used to swap EVERY dataVolume-backed volume to the boot output PVC, so an
// additional data disk (e.g. "supplemental") collapsed onto the boot disk's
// captured PVC. The boot disk must map to the output PVC while each additional
// disk keeps its own blank DV-backed PVC.
func TestRebuildVolumes_MultiDisk(t *testing.T) {
	tp := &TemplateProvisioner{}
	vm := templateVMWithVolumes([]any{
		map[string]any{
			"name":       "rootdisk",
			"dataVolume": map[string]any{"name": "bld-server"},
		},
		map[string]any{
			"name":       "supplemental",
			"dataVolume": map[string]any{"name": "bld-server-supplemental"},
		},
		map[string]any{
			"name":                  "iso0",
			"persistentVolumeClaim": map[string]any{"claimName": "bld-iso-clone"},
		},
	})

	tp.rebuildVolumes(vm, "bld", "server", "bld-out-server", "rootdisk")

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 2 {
		t.Fatalf("got %d volumes, want 2 (iso dropped)", len(volumes))
	}

	if got := claimName(t, volumeByName(t, vm, "rootdisk")); got != "bld-out-server" {
		t.Errorf("rootdisk claim = %q, want bld-out-server", got)
	}
	if got := claimName(t, volumeByName(t, vm, "supplemental")); got != "bld-out-server-supplemental" {
		t.Errorf("supplemental claim = %q, want bld-out-server-supplemental (its own captured output PVC, not the boot PVC)", got)
	}
	if volumeByName(t, vm, "iso0") != nil {
		t.Error("iso0 volume should have been dropped")
	}
}

// TestRebuildVolumes_NonRootBootDisk verifies the boot disk is identified by
// name (not hardcoded "rootdisk"), so a build whose boot disk is named
// differently still maps to the output PVC.
func TestRebuildVolumes_NonRootBootDisk(t *testing.T) {
	tp := &TemplateProvisioner{}
	vm := templateVMWithVolumes([]any{
		map[string]any{
			"name":       "os",
			"dataVolume": map[string]any{"name": "bld-server"},
		},
		map[string]any{
			"name":       "data",
			"dataVolume": map[string]any{"name": "bld-server-data"},
		},
	})

	tp.rebuildVolumes(vm, "bld", "server", "bld-out-server", "os")

	if got := claimName(t, volumeByName(t, vm, "os")); got != "bld-out-server" {
		t.Errorf("os (boot) claim = %q, want bld-out-server", got)
	}
	if got := claimName(t, volumeByName(t, vm, "data")); got != "bld-out-server-data" {
		t.Errorf("data claim = %q, want bld-out-server-data", got)
	}
}

// TestRebuildVolumes_CloudInitStripsUserData confirms cloud-init userData is
// removed while networkData is preserved.
func TestRebuildVolumes_CloudInitStripsUserData(t *testing.T) {
	tp := &TemplateProvisioner{}
	vm := templateVMWithVolumes([]any{
		map[string]any{
			"name":       "rootdisk",
			"dataVolume": map[string]any{"name": "bld-server"},
		},
		map[string]any{
			"name": "cloudinit",
			"cloudInitNoCloud": map[string]any{
				"userData":    "#cloud-config\nprovision: true",
				"networkData": "version: 2",
			},
		},
	})

	tp.rebuildVolumes(vm, "bld", "server", "bld-out-server", "rootdisk")

	ci := volumeByName(t, vm, "cloudinit")
	if ci == nil {
		t.Fatal("cloudinit volume dropped, want kept (has networkData)")
	}
	data := ci["cloudInitNoCloud"].(map[string]any)
	if _, ok := data["userData"]; ok {
		t.Error("userData should have been stripped")
	}
	if _, ok := data["networkData"]; !ok {
		t.Error("networkData should have been kept")
	}
}

var vmGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}

func vmScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(vmGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: vmGVK.Group, Version: vmGVK.Version, Kind: vmGVK.Kind + "List",
	}, &unstructured.UnstructuredList{})
	return s
}

// TestConvertToTemplate_PreservesInvisibleAnnotation confirms convertToTemplate
// merges (rather than replaces) VM-level annotations, so a ruddervirt.io/invisible
// annotation stamped by buildVM survives the build→template conversion. Labels
// are rebuilt wholesale here (see convertToTemplate), but annotations are
// read-merge-written — this is the invariant AnnotationInvisible relies on.
func TestConvertToTemplate_PreservesInvisibleAnnotation(t *testing.T) {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName("bld-webserver")
	vm.SetNamespace("vm-bld")
	vm.SetAnnotations(map[string]string{v1alpha1.AnnotationInvisible: valueTrue})

	cl := fake.NewClientBuilder().WithScheme(vmScheme(t)).WithObjects(vm).Build()
	tp := &TemplateProvisioner{Client: cl}

	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld"},
	}
	vmSpec := &v1alpha1.BuildVM{Name: "webserver", Source: v1alpha1.BuildSource{Blank: true}, Invisible: true}

	converted, err := tp.convertToTemplate(context.Background(), build, "vm-bld", "bld-webserver", "bld-out-webserver", vmSpec, "")
	if err != nil {
		t.Fatalf("convertToTemplate: %v", err)
	}
	if !converted {
		t.Fatal("expected VM to be converted")
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(vmGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bld-webserver", Namespace: "vm-bld"}, got); err != nil {
		t.Fatalf("getting converted VM: %v", err)
	}
	if annotation := got.GetAnnotations()[v1alpha1.AnnotationInvisible]; annotation != valueTrue {
		t.Errorf("annotations[%s] = %q, want %q", v1alpha1.AnnotationInvisible, annotation, valueTrue)
	}
}
