package clone

import (
	"context"
	"encoding/json"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

func TestEnsureClonePVC_EFIVarsNaming(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        EFIVarsVolumeName,
		SourceVMShortName: "module",
		SnapshotName:      "snap-efivars",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "256Mi",
	}

	_, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system")
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
	vm := &VolumeManager{Client: c}

	state := &v1alpha1.CloneVolumeStatus{
		VolumeName:        "rootdisk",
		SourceVMShortName: "module",
		SnapshotName:      "snap-rootdisk",
		StorageClassName:  "rook-ceph-block",
		RequestedStorage:  "37Gi",
	}

	_, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", state, "ruddervirt-system")
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

// TestEnsureClonePVC_MultiDiskUnique guards the multi-disk regression: every
// non-efivars disk on a VM used to collapse onto a single "{cloneID}-out-{vm}"
// PVC name, so the second disk reused the first disk's PVC and the cloned VM
// booted with two volumes pointing at the same claim. Each disk must now get a
// distinct, volume-scoped PVC name.
func TestEnsureClonePVC_MultiDiskUnique(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
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
		if _, err := vm.EnsureClonePVC(context.Background(), "ns-abc123", s, "ruddervirt-system"); err != nil {
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

	err := ensureVirtualMachine(context.Background(), c, templateVM, "ns-clone1", "ruddervirt-system", "test-source", volumeStates, nil, nil)
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

	err := ensureVirtualMachine(context.Background(), c, templateVM, "ns-clone2", "ruddervirt-system", "test-source", volumeStates, nil, nil)
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

	var rootState, efiState *v1alpha1.CloneVolumeStatus
	for i := range states {
		switch states[i].VolumeName {
		case "rootdisk":
			rootState = &states[i]
		case EFIVarsVolumeName:
			efiState = &states[i]
		}
	}

	if rootState == nil {
		t.Fatal("rootdisk volume state not found")
	}
	if rootState.SourcePVCName != "vm-build789-out-module" {
		t.Errorf("rootdisk PVC = %s, want vm-build789-out-module", rootState.SourcePVCName)
	}

	if efiState == nil {
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

func TestBuildInitialVolumeStates_NoEFIWithoutFirmware(t *testing.T) {
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
