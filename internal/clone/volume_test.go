package clone

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHookSidecarsJSON(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE", "ghcr.io/ruddervirt/aileron:abc123")

	got, err := hookSidecarsJSON("my-clone-module-efivars")
	if err != nil {
		t.Fatal(err)
	}

	var hooks []map[string]any
	if err := json.Unmarshal([]byte(got), &hooks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("got %d hooks, want 1", len(hooks))
	}

	hook := hooks[0]
	if hook["image"] != "ghcr.io/ruddervirt/aileron/sidecar:abc123" {
		t.Errorf("image = %v, want aileron/sidecar:abc123", hook["image"])
	}
	pvc, ok := hook["pvc"].(map[string]any)
	if !ok {
		t.Fatal("pvc field missing or not a map")
	}
	if pvc["name"] != "my-clone-module-efivars" {
		t.Errorf("pvc.name = %v, want my-clone-module-efivars", pvc["name"])
	}
	if pvc["volumePath"] != "/efivars" {
		t.Errorf("pvc.volumePath = %v", pvc["volumePath"])
	}
	if pvc["sharedComputePath"] != "/var/run/efivars" {
		t.Errorf("pvc.sharedComputePath = %v", pvc["sharedComputePath"])
	}
}

func TestHookSidecarsJSON_DefaultImage(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE", "")

	got, err := hookSidecarsJSON("test-pvc")
	if err != nil {
		t.Fatal(err)
	}

	var hooks []map[string]any
	if err := json.Unmarshal([]byte(got), &hooks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if hooks[0]["image"] != "ghcr.io/ruddervirt/aileron/sidecar:latest" {
		t.Errorf("image = %v, want default sidecar image", hooks[0]["image"])
	}
}

// cloneTestNS is the shared namespace used across clone volume tests; template and
// clone resources live in the operator namespace.
const cloneTestNS = "ruddervirt-system"

// readySnapshot builds a VolumeSnapshot unstructured with status.readyToUse=true so
// EnsureClonePVC's pre-create validation accepts it as a live data source.
func readySnapshot(name string) *unstructured.Unstructured {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	snap.SetName(name)
	snap.SetNamespace(cloneTestNS)
	_ = unstructured.SetNestedField(snap.Object, true, "status", "readyToUse")
	return snap
}

// seedSnapshot creates a ready snapshot in the fake client so a subsequent
// EnsureClonePVC call gets past snapshot validation.
func seedSnapshot(t *testing.T, c client.Client, name string) {
	t.Helper()
	if err := c.Create(context.Background(), readySnapshot(name)); err != nil {
		t.Fatalf("seeding snapshot %s: %v", name, err)
	}
}

func TestEnsureClonePVC_EFIVarsNaming(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	seedSnapshot(t, c, "snap-efivars")
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        EFIVarsVolumeName,
		SourceVMShortName: "module",
		SnapshotName:      "snap-efivars",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "256Mi",
	}

	_, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if state.PersistentVolumeClaimName != "ns-abc123-module-efivars" {
		t.Errorf("PVC name = %s, want ns-abc123-module-efivars", state.PersistentVolumeClaimName)
	}
}

func TestEnsureClonePVC_DiskNaming(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	seedSnapshot(t, c, "snap-rootdisk")
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "rootdisk",
		SourceVMShortName: "module",
		SnapshotName:      "snap-rootdisk",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "37Gi",
	}

	_, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if state.PersistentVolumeClaimName != "ns-abc123-module-rootdisk" {
		t.Errorf("PVC name = %s, want ns-abc123-module-rootdisk", state.PersistentVolumeClaimName)
	}

	// Verify CDI content-type annotation is set.
	created := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "ns-abc123-module-rootdisk", Namespace: "ruddervirt-system",
	}, created); err != nil {
		t.Fatal(err)
	}
	if v := created.Annotations["cdi.kubevirt.io/storage.contentType"]; v != "kubevirt" {
		t.Errorf("CDI contentType annotation = %q, want kubevirt", v)
	}
}

func TestEnsureClonePVC_StampsExpiresAt(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	seedSnapshot(t, c, "snap-expiry")
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "rootdisk",
		SourceVMShortName: "module",
		SnapshotName:      "snap-expiry",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "37Gi",
	}
	expiresAt := metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

	if _, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, cloneTestNS, nil, &expiresAt); err != nil {
		t.Fatal(err)
	}

	created := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: state.PersistentVolumeClaimName, Namespace: cloneTestNS,
	}, created); err != nil {
		t.Fatal(err)
	}
	if v := created.Annotations[v1alpha1.AnnotationExpiresAt]; v != "2026-09-01T00:00:00Z" {
		t.Errorf("expires-at annotation = %q, want 2026-09-01T00:00:00Z", v)
	}
}

func TestEnsureClonePVC_NilExpiresAtLeavesAnnotationUnset(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	seedSnapshot(t, c, "snap-no-expiry")
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "rootdisk",
		SourceVMShortName: "module",
		SnapshotName:      "snap-no-expiry",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "37Gi",
	}

	if _, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, cloneTestNS, nil, nil); err != nil {
		t.Fatal(err)
	}

	created := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: state.PersistentVolumeClaimName, Namespace: cloneTestNS,
	}, created); err != nil {
		t.Fatal(err)
	}
	if _, ok := created.Annotations[v1alpha1.AnnotationExpiresAt]; ok {
		t.Errorf("expires-at annotation should be unset when expiresAt is nil, got %q", created.Annotations[v1alpha1.AnnotationExpiresAt])
	}
}

// TestEnsureClonePVC_MultiDiskUnique guards the multi-disk regression: every
// non-efivars disk on a VM used to collapse onto a single "{cloneID}-out-{vm}"
// PVC name, so the second disk reused the first disk's PVC and the cloned VM
// booted with two volumes pointing at the same claim. Each disk must now get a
// distinct, volume-scoped PVC name.
func TestEnsureClonePVC_MultiDiskUnique(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	seedSnapshot(t, c, "snap-rootdisk")
	seedSnapshot(t, c, "snap-supplemental")
	vm := &VolumeManager{Client: c}

	boot := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "rootdisk",
		SourceVMShortName: "server",
		SnapshotName:      "snap-rootdisk",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "37Gi",
	}
	extra := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "supplemental",
		SourceVMShortName: "server",
		SnapshotName:      "snap-supplemental",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "5Gi",
	}

	for _, s := range []*v1alpha1.CloneVolumeStatus{boot, extra} {
		if _, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", s, "ruddervirt-system", nil, nil); err != nil {
			t.Fatalf("EnsureClonePVC(%s): %v", s.VolumeName, err)
		}
	}

	if boot.PersistentVolumeClaimName == extra.PersistentVolumeClaimName {
		t.Fatalf("boot and supplemental collapsed onto the same PVC %q", boot.PersistentVolumeClaimName)
	}
	if boot.PersistentVolumeClaimName != "ns-abc123-server-rootdisk" {
		t.Errorf("boot PVC = %s, want ns-abc123-server-rootdisk", boot.PersistentVolumeClaimName)
	}
	if extra.PersistentVolumeClaimName != "ns-abc123-server-supplemental" {
		t.Errorf("supplemental PVC = %s, want ns-abc123-server-supplemental", extra.PersistentVolumeClaimName)
	}

	// The supplemental PVC must be materialised from its OWN snapshot, not the
	// boot snapshot.
	extraPVC := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "ns-abc123-server-supplemental", Namespace: "ruddervirt-system",
	}, extraPVC); err != nil {
		t.Fatal(err)
	}
	if extraPVC.Spec.DataSource == nil || extraPVC.Spec.DataSource.Name != "snap-supplemental" {
		t.Errorf("supplemental PVC dataSource = %+v, want snapshot snap-supplemental", extraPVC.Spec.DataSource)
	}
}

func TestRewireVMVolumes_IgnoresEFIVars(t *testing.T) {
	vm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata":   map[string]any{"name": "tpl-vm"},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"volumes": []any{
							map[string]any{
								"name": "rootdisk",
								"persistentVolumeClaim": map[string]any{
									"claimName": "tpl-out-module",
								},
							},
						},
					},
				},
			},
		},
	}

	states := []v1alpha1.CloneVolumeStatus{
		{
			VolumeName:                "rootdisk",
			SourceVMName:              "tpl-vm",
			PersistentVolumeClaimName: "clone-out-module",
		},
		{
			VolumeName:                EFIVarsVolumeName,
			SourceVMName:              "tpl-vm",
			PersistentVolumeClaimName: "clone-module-efivars",
		},
	}

	if err := RewireVMVolumes(vm, states); err != nil {
		t.Fatal(err)
	}

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 1 {
		t.Fatalf("got %d volumes, want 1 (efivars should not appear)", len(volumes))
	}
	volMap := volumes[0].(map[string]any)
	pvc := volMap["persistentVolumeClaim"].(map[string]any)
	if pvc["claimName"] != "clone-out-module" {
		t.Errorf("rootdisk PVC = %v, want clone-out-module", pvc["claimName"])
	}
}

// findVolumeState returns the volume state matching name by value, rather
// than handing back a pointer into the slice — this keeps callers on plain
// value semantics instead of aliasing loop-scoped addresses.
func findVolumeState(states []v1alpha1.CloneVolumeStatus, name string) (v1alpha1.CloneVolumeStatus, bool) {
	for i := range states {
		if states[i].VolumeName == name {
			return states[i], true
		}
	}
	return v1alpha1.CloneVolumeStatus{}, false
}

func makeTemplateVM(buildID, vmShortName string, withEFI bool) *unstructured.Unstructured {
	vmName := buildID + "-" + vmShortName
	vm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":      vmName,
				"namespace": "ruddervirt-system",
				"labels": map[string]any{
					"ruddervirt.io/build-id":  buildID,
					"ruddervirt.io/vm":        vmShortName,
					"ruddervirt.io/component": "template",
				},
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"domain": map[string]any{
							"cpu": map[string]any{"cores": int64(2)},
						},
						"volumes": []any{
							map[string]any{
								"name": "rootdisk",
								"persistentVolumeClaim": map[string]any{
									"claimName": buildID + "-out-" + vmShortName,
								},
							},
						},
					},
					"metadata": map[string]any{
						"labels": map[string]any{},
					},
				},
			},
		},
	}
	if withEFI {
		_ = unstructured.SetNestedField(vm.Object, map[string]any{
			"bootloader": map[string]any{
				"efi": map[string]any{"secureBoot": false},
			},
		}, "spec", "template", "spec", "domain", "firmware")
		// BuildInitialVolumeStates detects an efivars-style PVC via the
		// hookSidecars annotation's "pvc" key (set by
		// internal/build.BuildHookSidecarsAnnotation whenever the build used
		// efiFirmware and/or floppy), not via the firmware block alone.
		hookJSON, _ := json.Marshal([]map[string]any{{
			"args":  []string{"--version", "v1alpha2"},
			"image": "ghcr.io/ruddervirt/aileron/sidecar:latest",
			"pvc": map[string]any{
				"name":              buildID + "-" + vmShortName + "-efivars",
				"volumePath":        "/efivars",
				"sharedComputePath": "/var/run/efivars",
			},
		}})
		_ = unstructured.SetNestedField(vm.Object, map[string]any{
			"hooks.kubevirt.io/hookSidecars": string(hookJSON),
		}, "spec", "template", "metadata", "annotations")
	}
	return vm
}

func TestEnsureVirtualMachine_SetsHookAnnotation(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE", "ghcr.io/ruddervirt/aileron:test")

	templateVM := makeTemplateVM("vm-build123", "module", true)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	volumeStates := []v1alpha1.CloneVolumeStatus{
		{
			VolumeName:                "rootdisk",
			SourceVMName:              "vm-build123-module",
			PersistentVolumeClaimName: "ns-clone1-out-module",
		},
		{
			VolumeName:                EFIVarsVolumeName,
			SourceVMName:              "vm-build123-module",
			PersistentVolumeClaimName: "ns-clone1-module-efivars",
		},
	}

	err := ensureVirtualMachine(context.Background(), c, templateVM, "ns-clone1", "ruddervirt-system", "test-source", volumeStates, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	created := &unstructured.Unstructured{}
	created.SetGroupVersionKind(vmGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      "ns-clone1-module",
		Namespace: "ruddervirt-system",
	}, created); err != nil {
		t.Fatal(err)
	}

	annotations, _, _ := unstructured.NestedStringMap(created.Object, "spec", "template", "metadata", "annotations")
	hookJSON, ok := annotations["hooks.kubevirt.io/hookSidecars"]
	if !ok {
		t.Fatal("hook sidecar annotation not set on clone VM")
	}

	var hooks []map[string]any
	if err := json.Unmarshal([]byte(hookJSON), &hooks); err != nil {
		t.Fatalf("invalid hook JSON: %v", err)
	}
	pvc := hooks[0]["pvc"].(map[string]any)
	if pvc["name"] != "ns-clone1-module-efivars" {
		t.Errorf("hook PVC = %v, want ns-clone1-module-efivars", pvc["name"])
	}
}

func TestEnsureVirtualMachine_NoHookWithoutEFI(t *testing.T) {
	templateVM := makeTemplateVM("vm-build456", "server", false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	volumeStates := []v1alpha1.CloneVolumeStatus{
		{
			VolumeName:                "rootdisk",
			SourceVMName:              "vm-build456-server",
			PersistentVolumeClaimName: "ns-clone2-out-server",
		},
	}

	err := ensureVirtualMachine(context.Background(), c, templateVM, "ns-clone2", "ruddervirt-system", "test-source", volumeStates, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	created := &unstructured.Unstructured{}
	created.SetGroupVersionKind(vmGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      "ns-clone2-server",
		Namespace: "ruddervirt-system",
	}, created); err != nil {
		t.Fatal(err)
	}

	annotations, _, _ := unstructured.NestedStringMap(created.Object, "spec", "template", "metadata", "annotations")
	if _, ok := annotations["hooks.kubevirt.io/hookSidecars"]; ok {
		t.Error("hook sidecar annotation should not be set on non-EFI clone VM")
	}
}

func TestEnsureVirtualMachine_StampsExpiresAtAnnotation(t *testing.T) {
	templateVM := makeTemplateVM("vm-build789", "worker", false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	volumeStates := []v1alpha1.CloneVolumeStatus{
		{
			VolumeName:                "rootdisk",
			SourceVMName:              "vm-build789-worker",
			PersistentVolumeClaimName: "ns-clone3-out-worker",
		},
	}

	expiresAt := metav1.NewTime(time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC))
	err := ensureVirtualMachine(context.Background(), c, templateVM, "ns-clone3", "ruddervirt-system", "test-source", volumeStates, nil, nil, &expiresAt)
	if err != nil {
		t.Fatal(err)
	}

	created := &unstructured.Unstructured{}
	created.SetGroupVersionKind(vmGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      "ns-clone3-worker",
		Namespace: "ruddervirt-system",
	}, created); err != nil {
		t.Fatal(err)
	}

	want := "2026-08-22T00:00:00Z"
	if got := created.GetAnnotations()[v1alpha1.AnnotationExpiresAt]; got != want {
		t.Errorf("expires-at annotation = %q, want %q", got, want)
	}
}

func TestEnsureVirtualMachine_NoExpiresAtAnnotationWhenNil(t *testing.T) {
	templateVM := makeTemplateVM("vm-build790", "worker2", false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	volumeStates := []v1alpha1.CloneVolumeStatus{
		{
			VolumeName:                "rootdisk",
			SourceVMName:              "vm-build790-worker2",
			PersistentVolumeClaimName: "ns-clone4-out-worker2",
		},
	}

	err := ensureVirtualMachine(context.Background(), c, templateVM, "ns-clone4", "ruddervirt-system", "test-source", volumeStates, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	created := &unstructured.Unstructured{}
	created.SetGroupVersionKind(vmGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      "ns-clone4-worker2",
		Namespace: "ruddervirt-system",
	}, created); err != nil {
		t.Fatal(err)
	}

	if _, ok := created.GetAnnotations()[v1alpha1.AnnotationExpiresAt]; ok {
		t.Error("expires-at annotation should not be set when expiresAt is nil")
	}
}

func TestEnsureVirtualMachine_CarriesInvisibleAnnotation(t *testing.T) {
	templateVM := makeTemplateVM("vm-build800", "webserver", false)
	templateVM.SetAnnotations(map[string]string{v1alpha1.AnnotationInvisible: annotationTrue})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	volumeStates := []v1alpha1.CloneVolumeStatus{
		{
			VolumeName:                "rootdisk",
			SourceVMName:              "vm-build800-webserver",
			PersistentVolumeClaimName: "ns-clone5-out-webserver",
		},
	}

	err := ensureVirtualMachine(context.Background(), c, templateVM, "ns-clone5", "ruddervirt-system", "test-source", volumeStates, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	created := &unstructured.Unstructured{}
	created.SetGroupVersionKind(vmGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      "ns-clone5-webserver",
		Namespace: "ruddervirt-system",
	}, created); err != nil {
		t.Fatal(err)
	}

	if got := created.GetAnnotations()[v1alpha1.AnnotationInvisible]; got != annotationTrue {
		t.Errorf("invisible annotation = %q, want %q", got, annotationTrue)
	}
}

func TestCheckVMsReady_PopulatesInvisibleFromTemplate(t *testing.T) {
	visibleTemplate := makeTemplateVM("vm-build900", "app", false)
	invisibleTemplate := makeTemplateVM("vm-build900", "webserver", false)
	invisibleTemplate.SetAnnotations(map[string]string{v1alpha1.AnnotationInvisible: annotationTrue})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	statuses, allReady, err := CheckVMsReady(context.Background(), c, []*unstructured.Unstructured{visibleTemplate, invisibleTemplate}, "ns-clone6", "ruddervirt-system")
	if err != nil {
		t.Fatal(err)
	}
	if allReady {
		t.Error("allReady should be false — neither clone VM was created")
	}

	byName := make(map[string]v1alpha1.ClonedVMStatus, len(statuses))
	for _, s := range statuses {
		byName[s.Name] = s
	}

	if s := byName["ns-clone6-app"]; s.Invisible {
		t.Error("app VM should not be invisible")
	}
	if s := byName["ns-clone6-webserver"]; !s.Invisible {
		t.Error("webserver VM should be invisible")
	}
}

func TestCheckVMsReady_InvisibleTrueWhenCloneVMAlreadyExists(t *testing.T) {
	invisibleTemplate := makeTemplateVM("vm-build901", "webserver", false)
	invisibleTemplate.SetAnnotations(map[string]string{v1alpha1.AnnotationInvisible: annotationTrue})

	cloneVM := &unstructured.Unstructured{}
	cloneVM.SetGroupVersionKind(vmGVK)
	cloneVM.SetName("ns-clone7-webserver")
	cloneVM.SetNamespace("ruddervirt-system")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cloneVM).
		Build()

	statuses, allReady, err := CheckVMsReady(context.Background(), c, []*unstructured.Unstructured{invisibleTemplate}, "ns-clone7", "ruddervirt-system")
	if err != nil {
		t.Fatal(err)
	}
	if !allReady {
		t.Error("allReady should be true — the clone VM exists")
	}
	if len(statuses) != 1 || !statuses[0].Invisible {
		t.Errorf("statuses = %+v, want a single invisible=true entry", statuses)
	}
}

func TestBuildInitialVolumeStates_DetectsEFIPVC(t *testing.T) {
	templateVM := makeTemplateVM("vm-build789", "module", true)

	efiPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vm-build789-module-efivars",
			Namespace: "ruddervirt-system",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       "pv-efi-001",
			StorageClassName: new("rook-ceph-block"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("256Mi"),
				},
			},
		},
	}

	rootPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vm-build789-out-module",
			Namespace: "ruddervirt-system",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       "pv-root-001",
			StorageClassName: new("rook-ceph-block"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("37Gi"),
				},
			},
		},
	}

	efiPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-efi-001"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "rook-ceph.rbd.csi.ceph.com",
				},
			},
		},
	}

	rootPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-root-001"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "rook-ceph.rbd.csi.ceph.com",
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rootPVC, efiPVC, rootPV, efiPV).
		Build()

	sm := &SnapshotManager{Client: c}

	states, err := sm.BuildInitialVolumeStates(
		context.Background(),
		[]*unstructured.Unstructured{templateVM},
		"ruddervirt-system",
	)
	if err != nil {
		t.Fatal(err)
	}

	rootState, ok := findVolumeState(states, "rootdisk")
	if !ok {
		t.Fatal("rootdisk volume state not found")
	}
	if rootState.SourcePVCName != "vm-build789-out-module" {
		t.Errorf("rootdisk PVC = %s, want vm-build789-out-module", rootState.SourcePVCName)
	}

	efiState, ok := findVolumeState(states, EFIVarsVolumeName)
	if !ok {
		t.Fatal("efivars volume state not found")
	}
	if efiState.SourcePVCName != "vm-build789-module-efivars" {
		t.Errorf("efivars PVC = %s, want vm-build789-module-efivars", efiState.SourcePVCName)
	}
	if efiState.CSIDriver != "rook-ceph.rbd.csi.ceph.com" {
		t.Errorf("efivars CSI = %s", efiState.CSIDriver)
	}
	if efiState.RequestedStorage != "256Mi" {
		t.Errorf("efivars storage = %s, want 256Mi", efiState.RequestedStorage)
	}
}

func TestBuildInitialVolumeStates_NoEFIWithoutHookAnnotation(t *testing.T) {
	templateVM := makeTemplateVM("vm-build000", "server", false)

	rootPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vm-build000-out-server",
			Namespace: "ruddervirt-system",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       "pv-root-002",
			StorageClassName: new("rook-ceph-block"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("37Gi"),
				},
			},
		},
	}

	rootPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-root-002"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "rook-ceph.rbd.csi.ceph.com",
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rootPVC, rootPV).
		Build()

	sm := &SnapshotManager{Client: c}

	states, err := sm.BuildInitialVolumeStates(
		context.Background(),
		[]*unstructured.Unstructured{templateVM},
		"ruddervirt-system",
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range states {
		if s.VolumeName == EFIVarsVolumeName {
			t.Error("efivars state should not exist for non-EFI template VM")
		}
	}
	if len(states) != 1 {
		t.Errorf("got %d states, want 1 (rootdisk only)", len(states))
	}
}

// TestBuildInitialVolumeStates_DetectsEFIPVC_FloppyOnly is the direct
// regression test for the "floppy without efiFirmware" bug: a template VM
// with a hookSidecars/pvc annotation (set because the build used floppy) but
// no spec.domain.firmware.bootloader.efi block must still get its efivars
// PVC captured for cloning — otherwise clones inherit a stale,
// build-namespace-pointing hookSidecars annotation and fail to mount/start.
func TestBuildInitialVolumeStates_DetectsEFIPVC_FloppyOnly(t *testing.T) {
	buildID, vmShortName := "vm-build999", "installer"
	templateVM := makeTemplateVM(buildID, vmShortName, false)
	hookJSON, _ := json.Marshal([]map[string]any{{
		"args":  []string{"--version", "v1alpha2", "--floppy"},
		"image": "ghcr.io/ruddervirt/aileron/sidecar:latest",
		"pvc": map[string]any{
			"name":              buildID + "-" + vmShortName + "-efivars",
			"volumePath":        "/efivars",
			"sharedComputePath": "/var/run/efivars",
		},
	}})
	_ = unstructured.SetNestedField(templateVM.Object, map[string]any{
		"hooks.kubevirt.io/hookSidecars": string(hookJSON),
	}, "spec", "template", "metadata", "annotations")

	efiPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildID + "-" + vmShortName + "-efivars",
			Namespace: "ruddervirt-system",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       "pv-efi-002",
			StorageClassName: new("rook-ceph-block"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("256Mi"),
				},
			},
		},
	}
	rootPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildID + "-out-" + vmShortName,
			Namespace: "ruddervirt-system",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       "pv-root-003",
			StorageClassName: new("rook-ceph-block"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("37Gi"),
				},
			},
		},
	}
	efiPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-efi-002"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "rook-ceph.rbd.csi.ceph.com"},
			},
		},
	}
	rootPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-root-003"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "rook-ceph.rbd.csi.ceph.com"},
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rootPVC, efiPVC, rootPV, efiPV).
		Build()

	sm := &SnapshotManager{Client: c}
	states, err := sm.BuildInitialVolumeStates(
		context.Background(),
		[]*unstructured.Unstructured{templateVM},
		"ruddervirt-system",
	)
	if err != nil {
		t.Fatal(err)
	}

	efiState, ok := findVolumeState(states, EFIVarsVolumeName)
	if !ok {
		t.Fatal("efivars volume state not found for floppy-only (non-EFI) template VM")
	}
	if efiState.SourcePVCName != buildID+"-"+vmShortName+"-efivars" {
		t.Errorf("efivars PVC = %s, want %s-%s-efivars", efiState.SourcePVCName, buildID, vmShortName)
	}
}

// TestHookSidecarsJSON_NeverEmitsFloppyArg documents the invariant the
// default-off fix relies on: hookSidecarsJSON has no floppy awareness at
// all, so a clone's regenerated hook annotation can never carry --floppy,
// regardless of what the template's original annotation contained.
func TestHookSidecarsJSON_NeverEmitsFloppyArg(t *testing.T) {
	got, err := hookSidecarsJSON("some-efivars-pvc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "--floppy") {
		t.Errorf("hookSidecarsJSON emitted --floppy; got: %s", got)
	}
}

// TestEnsureClonePVC_MissingSnapshotFailsFast is the core regression: when the
// snapshot data source does not exist, EnsureClonePVC must fail fast (ErrSnapshotUnusable)
// and must NOT create a PVC that could only spin Pending forever.
func TestEnsureClonePVC_MissingSnapshotFailsFast(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "rootdisk",
		SourceVMShortName: "module",
		SnapshotName:      "snap-gone", // never created
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "37Gi",
	}

	_, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system", nil, nil)
	if err == nil {
		t.Fatal("expected error for missing snapshot, got nil")
	}
	if !IsSnapshotUnusable(err) {
		t.Fatalf("error = %v, want ErrSnapshotUnusable", err)
	}

	// No PVC must have been created.
	got := &corev1.PersistentVolumeClaim{}
	err = c.Get(context.Background(), types.NamespacedName{
		Name: "ns-abc123-module-rootdisk", Namespace: "ruddervirt-system",
	}, got)
	if err == nil {
		t.Fatal("a PVC was created against a missing snapshot; want none")
	}
}

// TestEnsureClonePVC_TerminatingSnapshotFailsFast covers the exact incident trigger:
// the snapshot exists but is being deleted ("snapshot is currently being deleted").
func TestEnsureClonePVC_TerminatingSnapshotFailsFast(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	// Create the snapshot with a finalizer, then delete it so the fake client keeps
	// it with a deletionTimestamp set (terminating).
	snap := readySnapshot("snap-terminating")
	snap.SetFinalizers([]string{"test/hold"})
	if err := c.Create(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), snap); err != nil {
		t.Fatal(err)
	}

	vm := &VolumeManager{Client: c}
	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "rootdisk",
		SourceVMShortName: "module",
		SnapshotName:      "snap-terminating",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "37Gi",
	}

	_, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system", nil, nil)
	if !IsSnapshotUnusable(err) {
		t.Fatalf("error = %v, want ErrSnapshotUnusable", err)
	}
	got := &corev1.PersistentVolumeClaim{}
	if c.Get(context.Background(), types.NamespacedName{
		Name: "ns-abc123-module-rootdisk", Namespace: "ruddervirt-system",
	}, got) == nil {
		t.Fatal("a PVC was created against a terminating snapshot; want none")
	}
}

// TestEnsureClonePVC_SetsOwnerReference verifies clone PVCs are owned by the clone's
// VirtualMachineNamespace CR, so deleting the clone root garbage-collects its storage.
func TestEnsureClonePVC_SetsOwnerReference(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	seedSnapshot(t, c, "snap-rootdisk")
	vm := &VolumeManager{Client: c}

	owner := &v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-abc123", Namespace: "ruddervirt-system", UID: "owner-uid-1"},
	}
	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "rootdisk",
		SourceVMShortName: "module",
		SnapshotName:      "snap-rootdisk",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "37Gi",
	}

	if _, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system", owner, nil); err != nil {
		t.Fatal(err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "ns-abc123-module-rootdisk", Namespace: "ruddervirt-system",
	}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %d, want 1", len(got.OwnerReferences))
	}
	ref := got.OwnerReferences[0]
	if ref.Kind != "VirtualMachineNamespace" || ref.Name != "ns-abc123" || ref.UID != "owner-uid-1" {
		t.Errorf("ownerReference = %+v, want VMNS ns-abc123/owner-uid-1", ref)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Error("ownerReference should be a controller reference")
	}
}

// TestEnsureClonePVC_ForeignBindReleased guards against the cross-namespace hijack:
// a clone PVC that statically adopted a PV aileron did not provision must be deleted
// (releasing that PV), not accepted as ready.
func TestEnsureClonePVC_ForeignBindReleased(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	boundPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ns-abc123-module-efivars",
			Namespace: "ruddervirt-system",
			Labels:    map[string]string{"ruddervirt.io/clone": "ns-abc123"},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: "pv-foreign"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	// A PV from another app: no CSI source, no provisioned-by annotation.
	foreignPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-foreign"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/mnt/data"},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(boundPVC, foreignPV).Build()
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:                EFIVarsVolumeName,
		SourceVMShortName:         "module",
		PersistentVolumeClaimName: "ns-abc123-module-efivars",
		SnapshotName:              "snap-efivars",
		CSIDriver:                 "rook-ceph.rbd.csi.ceph.com",
		StorageClassName:          "rook-ceph-block",
		RequestedStorage:          "256Mi",
	}

	ready, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("clone PVC bound to a foreign PV must not be reported ready")
	}
	// The mis-bound PVC must have been deleted to release the foreign PV.
	got := &corev1.PersistentVolumeClaim{}
	if c.Get(context.Background(), types.NamespacedName{
		Name: "ns-abc123-module-efivars", Namespace: "ruddervirt-system",
	}, got) == nil {
		t.Fatal("mis-bound clone PVC was not deleted")
	}
}

// TestEnsureClonePVC_LegitBindReady confirms a PVC bound to a PV that aileron's CSI
// driver provisioned is accepted as ready.
func TestEnsureClonePVC_LegitBindReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	boundPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ns-abc123-module-rootdisk",
			Namespace: "ruddervirt-system",
			Labels:    map[string]string{"ruddervirt.io/clone": "ns-abc123"},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: "pv-ceph"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	cephPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-ceph"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "rook-ceph.rbd.csi.ceph.com"},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(boundPVC, cephPV).Build()
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:                "rootdisk",
		SourceVMShortName:         "module",
		PersistentVolumeClaimName: "ns-abc123-module-rootdisk",
		SnapshotName:              "snap-rootdisk",
		CSIDriver:                 "rook-ceph.rbd.csi.ceph.com",
		StorageClassName:          "rook-ceph-block",
		RequestedStorage:          "37Gi",
	}

	ready, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("legitimately bound clone PVC should be ready")
	}
	if state.Phase != v1alpha1.CloneVolumePhasePVCBound {
		t.Errorf("state.Phase = %s, want %s", state.Phase, v1alpha1.CloneVolumePhasePVCBound)
	}
}
