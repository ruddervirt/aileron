package build

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// TestCleanupEphemeralResources_ISOCloneScopedToBuild guards against a
// regression where cleanupEphemeralResources deleted every DataVolume
// labeled iso-clone=true in the (shared) build namespace, not just the ones
// belonging to the build being cleaned up. Since all builds share the
// operator namespace, an unscoped delete collaterally destroys another
// build's in-flight ISO clone DV, which then surfaces as a KubeVirt
// "unable to find datavolume" boot failure for the unrelated build.
func TestCleanupEphemeralResources_ISOCloneScopedToBuild(t *testing.T) {
	ownDV := &unstructured.Unstructured{}
	ownDV.SetGroupVersionKind(dvGVK)
	ownDV.SetName("bld-a-server-iso0")
	ownDV.SetNamespace("ruddervirt-system")
	ownDV.SetLabels(map[string]string{
		"ruddervirt.io/iso-clone": "true",
		LabelBuildID:              "bld-a",
	})

	otherDV := &unstructured.Unstructured{}
	otherDV.SetGroupVersionKind(dvGVK)
	otherDV.SetName("bld-b-server-iso0")
	otherDV.SetNamespace("ruddervirt-system")
	otherDV.SetLabels(map[string]string{
		"ruddervirt.io/iso-clone": "true",
		LabelBuildID:              "bld-b",
	})

	cl := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(ownDV, otherDV).Build()
	tp := &TemplateProvisioner{Client: cl}

	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld-a", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld-a"},
	}

	if err := tp.cleanupEphemeralResources(context.Background(), build, "ruddervirt-system"); err != nil {
		t.Fatalf("cleanupEphemeralResources: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(dvGVK)
	err := cl.Get(context.Background(), types.NamespacedName{Name: "bld-a-server-iso0", Namespace: "ruddervirt-system"}, got)
	if err == nil {
		t.Error("own build's ISO clone DV should have been deleted, but still exists")
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("unexpected error getting own build's ISO clone DV: %v", err)
	}

	other := &unstructured.Unstructured{}
	other.SetGroupVersionKind(dvGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bld-b-server-iso0", Namespace: "ruddervirt-system"}, other); err != nil {
		t.Fatalf("other build's ISO clone DV should survive cleanup, got error: %v", err)
	}
}

// TestCleanupEphemeralResources_ReturnsErrorOnDeleteFailure confirms a transient
// delete failure (e.g. an API blip or a CDI finalizer race) is surfaced as an error
// instead of only logged — this phase runs exactly once per build, so a swallowed
// failure previously orphaned the resource (most notably the "-src-" DataVolume)
// permanently since nothing ever retried the sweep.
func TestCleanupEphemeralResources_ReturnsErrorOnDeleteFailure(t *testing.T) {
	srcDVName := BuildNameForBuildVMDataVolume("bld-a", "webserver")
	srcDV := &unstructured.Unstructured{}
	srcDV.SetGroupVersionKind(dvGVK)
	srcDV.SetName(srcDVName)
	srcDV.SetNamespace("ruddervirt-system")

	base := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(srcDV).Build()
	boom := errors.New("boom: transient API error")
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetName() == srcDVName {
				return boom
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	tp := &TemplateProvisioner{Client: cl}

	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld-a", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld-a"},
		Spec:       v1alpha1.VirtualMachineBuildSpec{VMs: []v1alpha1.BuildVM{{Name: "webserver"}}},
	}

	err := tp.cleanupEphemeralResources(context.Background(), build, "ruddervirt-system")
	if err == nil {
		t.Fatal("expected an error when the source DV delete fails, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("expected error chain to include the delete failure, got: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(dvGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Name: srcDVName, Namespace: "ruddervirt-system"}, got); err != nil {
		t.Errorf("source DV should still exist after a failed delete, got error: %v", err)
	}
}

// TestHandle_KeepsTemplateProvisioningPhaseOnCleanupFailure confirms a cleanup
// failure keeps the build in BuildPhaseTemplateProvisioning (so the controller
// requeues and retries the same phase) rather than transitioning to
// BuildPhaseFailed, which is terminal and would never retry.
func TestHandle_KeepsTemplateProvisioningPhaseOnCleanupFailure(t *testing.T) {
	srcDVName := BuildNameForBuildVMDataVolume("bld-a", "webserver")
	srcDV := &unstructured.Unstructured{}
	srcDV.SetGroupVersionKind(dvGVK)
	srcDV.SetName(srcDVName)
	srcDV.SetNamespace("ruddervirt-system")

	base := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(srcDV).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetName() == srcDVName {
				return errors.New("boom: transient API error")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	tp := &TemplateProvisioner{Client: cl}

	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld-a", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld-a", Phase: v1alpha1.BuildPhaseTemplateProvisioning},
		Spec:       v1alpha1.VirtualMachineBuildSpec{VMs: []v1alpha1.BuildVM{{Name: "webserver"}}},
	}

	phase, err := tp.Handle(context.Background(), build)
	if err == nil {
		t.Fatal("expected Handle to return an error when cleanup fails")
	}
	if phase != v1alpha1.BuildPhaseTemplateProvisioning {
		t.Errorf("phase = %q, want BuildPhaseTemplateProvisioning so the controller retries", phase)
	}
}

var vmGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}

// testVMUID is the shared fake UID used across efifirmware_test.go, template_test.go,
// and vm_test.go's ownEFIPVCByVM-related tests.
const testVMUID = "vm-uid-1"

func vmScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
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

// TestConvertToTemplate_OwnsEFIVarsPVCByVM confirms converting a build VM into a
// template also patches its efivars PVC to be owned by that VM — previously the PVC
// had no owner at all, so a template VM deleted directly (bypassing the parent
// VirtualMachineBuild CR's own delete/cleanup path, e.g. by an external watchdog)
// orphaned the PVC forever.
func TestConvertToTemplate_OwnsEFIVarsPVCByVM(t *testing.T) {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName("bld-webserver")
	vm.SetNamespace("vm-bld")
	vm.SetUID(testVMUID)

	efivars := efiPVCFor()

	cl := fake.NewClientBuilder().WithScheme(vmScheme(t)).WithObjects(vm, efivars).Build()
	tp := &TemplateProvisioner{Client: cl}

	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld"},
	}
	vmSpec := &v1alpha1.BuildVM{Name: "webserver", Source: v1alpha1.BuildSource{Blank: true}}

	if _, err := tp.convertToTemplate(context.Background(), build, "vm-bld", "bld-webserver", "bld-out-webserver", vmSpec, ""); err != nil {
		t.Fatalf("convertToTemplate: %v", err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bld-webserver-efivars", Namespace: "vm-bld"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].UID != testVMUID {
		t.Errorf("efivars PVC ownerReferences = %v, want owned by VM vm-uid-1", got.OwnerReferences)
	}
}

// TestConvertToTemplate_EFIOwnershipFailureDoesNotFailConversion confirms a transient
// failure patching the efivars PVC's ownership never fails template conversion —
// ownEFIPVCByVMBestEffort must swallow it, not propagate it up into
// BuildPhaseFailed (which is terminal and would permanently fail an otherwise
// successful build over a missed metadata patch).
func TestConvertToTemplate_EFIOwnershipFailureDoesNotFailConversion(t *testing.T) {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName("bld-webserver")
	vm.SetNamespace("vm-bld")
	vm.SetUID(testVMUID)

	efivars := efiPVCFor()

	base := fake.NewClientBuilder().WithScheme(vmScheme(t)).WithObjects(vm, efivars).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				return errors.New("boom: transient API error")
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	tp := &TemplateProvisioner{Client: cl}

	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld"},
	}
	vmSpec := &v1alpha1.BuildVM{Name: "webserver", Source: v1alpha1.BuildSource{Blank: true}}

	converted, err := tp.convertToTemplate(context.Background(), build, "vm-bld", "bld-webserver", "bld-out-webserver", vmSpec, "")
	if err != nil {
		t.Fatalf("convertToTemplate should not fail on a transient efivars-ownership error, got: %v", err)
	}
	if !converted {
		t.Fatal("expected VM to still be converted despite the efivars-ownership failure")
	}
}

// TestConvertToTemplate_RegeneratesHookAnnotationWithoutFloppy confirms the
// persisted template VM's hookSidecars annotation is regenerated (not copied
// verbatim from the live build VM), and always omits --floppy even when the
// build VM's own annotation carried it and vmSpec.Floppy was set — floppy is
// build-only, and this is the defense-in-depth guarantee that no enablement
// signal survives past the build into what clones inherit.
func TestConvertToTemplate_RegeneratesHookAnnotationWithoutFloppy(t *testing.T) {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName("bld-installer")
	vm.SetNamespace("vm-bld")
	_ = unstructured.SetNestedField(vm.Object, map[string]any{
		"hooks.kubevirt.io/hookSidecars": `[{"args":["--version","v1alpha2","--floppy"],"image":"ghcr.io/ruddervirt/aileron/sidecar:latest","pvc":{"name":"bld-installer-efivars","volumePath":"/efivars","sharedComputePath":"/var/run/efivars"}}]`,
	}, "spec", "template", "metadata", "annotations")

	cl := fake.NewClientBuilder().WithScheme(vmScheme(t)).WithObjects(vm).Build()
	tp := &TemplateProvisioner{Client: cl}

	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld"},
	}
	vmSpec := &v1alpha1.BuildVM{
		Name:   "installer",
		Source: v1alpha1.BuildSource{Blank: true},
		Floppy: &v1alpha1.Floppy{Files: []v1alpha1.FloppyFileRef{{Name: "Autounattend.xml"}}},
	}

	converted, err := tp.convertToTemplate(context.Background(), build, "vm-bld", "bld-installer", "bld-out-installer", vmSpec, "")
	if err != nil {
		t.Fatalf("convertToTemplate: %v", err)
	}
	if !converted {
		t.Fatal("expected VM to be converted")
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(vmGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bld-installer", Namespace: "vm-bld"}, got); err != nil {
		t.Fatalf("getting converted VM: %v", err)
	}

	annotations, _, _ := unstructured.NestedStringMap(got.Object, "spec", "template", "metadata", "annotations")
	hookJSON, ok := annotations["hooks.kubevirt.io/hookSidecars"]
	if !ok {
		t.Fatal("expected hook sidecar annotation to be regenerated on template VM")
	}
	if strings.Contains(hookJSON, "--floppy") {
		t.Errorf("template hookSidecars annotation carries --floppy forward: %s", hookJSON)
	}
	if !strings.Contains(hookJSON, `"pvc"`) {
		t.Errorf("template hookSidecars annotation missing PVC mount despite vmSpec.Floppy set: %s", hookJSON)
	}
}
