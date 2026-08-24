// SPDX-License-Identifier: GPL-3.0-only

package build

import (
	"context"
	"encoding/json"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestBuildHookSidecarsAnnotation_NeverEmitsFloppyArg confirms args is always
// exactly ["--version","v1alpha2"], with or without vmSpec.Floppy set.
// KubeVirt's sidecar-shim only recognizes --version on its own command line
// and rejects any other flag before ever exec'ing the hook binary, so a
// --floppy arg here would crash-loop every build using it (see
// cmd/sidecar/main.go — floppy injection is gated by file presence instead).
func TestBuildHookSidecarsAnnotation_NeverEmitsFloppyArg(t *testing.T) {
	vmSpec := &v1alpha1.BuildVM{
		Name:   "installer",
		Floppy: &v1alpha1.Floppy{Files: []v1alpha1.FloppyFileRef{{Name: "Autounattend.xml"}}},
	}

	got, err := BuildHookSidecarsAnnotation("bld123", vmSpec)
	if err != nil {
		t.Fatal(err)
	}

	var hooks []map[string]any
	if err := json.Unmarshal([]byte(got), &hooks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	args, _ := hooks[0]["args"].([]any)
	want := []any{"--version", "v1alpha2"}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// TestBuildHookSidecarsAnnotation_NoHookWithoutEFIOrFloppy confirms no hook
// annotation is emitted when neither EFIFirmware nor Floppy is configured.
func TestBuildHookSidecarsAnnotation_NoHookWithoutEFIOrFloppy(t *testing.T) {
	vmSpec := &v1alpha1.BuildVM{Name: "plain"}

	got, err := BuildHookSidecarsAnnotation("bld123", vmSpec)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty hook annotation, got: %s", got)
	}
}

func efiPVCFor() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "bld-webserver-efivars", Namespace: "vm-bld"},
	}
}

// TestOwnEFIPVCByVM_SetsOwnerReference confirms the efivars PVC ends up owned by the
// VM once it exists, closing the gap where the PVC previously had no owner at all and
// was only reaped when the parent VirtualMachineBuild CR was deleted through the
// normal reconcile path — not when the VM alone was removed.
func TestOwnEFIPVCByVM_SetsOwnerReference(t *testing.T) {
	scheme := vmScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	pvc := efiPVCFor()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName("bld-webserver")
	vm.SetNamespace("vm-bld")
	vm.SetUID(testVMUID)

	if err := ownEFIPVCByVM(context.Background(), cl, "vm-bld", "bld-webserver-efivars", vm); err != nil {
		t.Fatal(err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bld-webserver-efivars", Namespace: "vm-bld"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %v, want exactly 1", got.OwnerReferences)
	}
	ref := got.OwnerReferences[0]
	if ref.Kind != "VirtualMachine" || ref.Name != "bld-webserver" || ref.UID != testVMUID {
		t.Errorf("ownerReference = %+v, want VirtualMachine/bld-webserver/vm-uid-1", ref)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Error("ownerReference.Controller should be true so k8s GC cascades the delete")
	}
}

// TestOwnEFIPVCByVM_Idempotent confirms a second call against an already-owned PVC is
// a no-op — no duplicate ownerReference, and no spurious Update call (asserted via
// interceptor, not just inferred from the final state).
func TestOwnEFIPVCByVM_Idempotent(t *testing.T) {
	scheme := vmScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	pvc := efiPVCFor()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName("bld-webserver")
	vm.SetNamespace("vm-bld")
	vm.SetUID(testVMUID)

	if err := ownEFIPVCByVM(context.Background(), base, "vm-bld", "bld-webserver-efivars", vm); err != nil {
		t.Fatal(err)
	}

	cl := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			t.Fatalf("second call to ownEFIPVCByVM should be a no-op, but issued an Update on %T %s", obj, obj.GetName())
			return nil
		},
	})
	if err := ownEFIPVCByVM(context.Background(), cl, "vm-bld", "bld-webserver-efivars", vm); err != nil {
		t.Fatal(err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := base.Get(context.Background(), types.NamespacedName{Name: "bld-webserver-efivars", Namespace: "vm-bld"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %v, want exactly 1 after repeated calls", got.OwnerReferences)
	}
}

// TestOwnEFIPVCByVM_ReplacesStaleOwnerOnVMReplacement confirms a PVC already owned by
// a VirtualMachine UID that no longer matches (e.g. VMBooter deleted and recreated a
// drifted VM, which gets a fresh UID) gets its ownerReference replaced, not appended
// alongside the stale one — a PVC with two controller:true ownerReferences is rejected
// by the API server, so blind appending would break this on the very next VM
// replacement instead of just leaving one harmless stale reference.
func TestOwnEFIPVCByVM_ReplacesStaleOwnerOnVMReplacement(t *testing.T) {
	scheme := vmScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	pvc := efiPVCFor()
	pvc.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         "kubevirt.io/v1",
		Kind:               "VirtualMachine",
		Name:               "bld-webserver",
		UID:                "vm-uid-OLD",
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	newVM := &unstructured.Unstructured{}
	newVM.SetGroupVersionKind(vmGVK)
	newVM.SetName("bld-webserver")
	newVM.SetNamespace("vm-bld")
	newVM.SetUID("vm-uid-NEW")

	if err := ownEFIPVCByVM(context.Background(), cl, "vm-bld", "bld-webserver-efivars", newVM); err != nil {
		t.Fatal(err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bld-webserver-efivars", Namespace: "vm-bld"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %v, want exactly 1 (replaced, not appended)", got.OwnerReferences)
	}
	if got.OwnerReferences[0].UID != "vm-uid-NEW" {
		t.Errorf("ownerReference.UID = %q, want vm-uid-NEW", got.OwnerReferences[0].UID)
	}
}

// TestOwnEFIPVCByVM_MissingPVCIsNoOp confirms a not-yet-created (or already deleted)
// efivars PVC does not turn into an error — ensureEFIPVC/convertToTemplate call this
// unconditionally and must not fail the reconcile over a PVC that simply isn't there.
func TestOwnEFIPVCByVM_MissingPVCIsNoOp(t *testing.T) {
	scheme := vmScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName("bld-webserver")
	vm.SetNamespace("vm-bld")
	vm.SetUID(testVMUID)

	if err := ownEFIPVCByVM(context.Background(), cl, "vm-bld", "bld-webserver-efivars", vm); err != nil {
		t.Fatalf("missing PVC should be a no-op, got: %v", err)
	}
}
