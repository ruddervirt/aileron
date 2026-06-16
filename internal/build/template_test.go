package build

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	tp.rebuildVolumes(vm, "bld-out-server", "rootdisk")

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 2 {
		t.Fatalf("got %d volumes, want 2 (iso dropped)", len(volumes))
	}

	if got := claimName(t, volumeByName(t, vm, "rootdisk")); got != "bld-out-server" {
		t.Errorf("rootdisk claim = %q, want bld-out-server", got)
	}
	if got := claimName(t, volumeByName(t, vm, "supplemental")); got != "bld-server-supplemental" {
		t.Errorf("supplemental claim = %q, want bld-server-supplemental (its own PVC, not the boot PVC)", got)
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

	tp.rebuildVolumes(vm, "bld-out-server", "os")

	if got := claimName(t, volumeByName(t, vm, "os")); got != "bld-out-server" {
		t.Errorf("os (boot) claim = %q, want bld-out-server", got)
	}
	if got := claimName(t, volumeByName(t, vm, "data")); got != "bld-server-data" {
		t.Errorf("data claim = %q, want bld-server-data", got)
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

	tp.rebuildVolumes(vm, "bld-out-server", "rootdisk")

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
