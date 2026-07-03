/*
Copyright 2026.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/

package controller

import (
	"context"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const reaperNS = "ruddervirt-system"

// reaperScheme registers the typed aileron/core types plus the unstructured KubeVirt
// VirtualMachine list kind so the fake client can serve the reaper's VM lookup.
func reaperScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList"}, &unstructured.UnstructuredList{})
	return s
}

func clonePVC(name, cloneID string, phase corev1.PersistentVolumeClaimPhase, volumeName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: reaperNS,
			Labels:    map[string]string{cloneLabel: cloneID},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: volumeName},
		Status: corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

func vmReferencingPVC(name, claimName string) *unstructured.Unstructured {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"})
	vm.SetName(name)
	vm.SetNamespace(reaperNS)
	_ = unstructured.SetNestedSlice(vm.Object, []any{
		map[string]any{
			"name":                  "rootdisk",
			"persistentVolumeClaim": map[string]any{"claimName": claimName},
		},
	}, "spec", "template", "spec", "volumes")
	return vm
}

func newReaper(c client.Client) *PVCReaper {
	return &PVCReaper{Client: c, Reader: c}
}

func pvcExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	got := &corev1.PersistentVolumeClaim{}
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: reaperNS}, got)
	return err == nil
}

// TestReaper_DeletesOrphanPendingPVC covers the backlog: a Pending clone PVC whose
// clone has no live VM and no clone CR, and which no VM uses, must be collected.
func TestReaper_DeletesOrphanPendingPVC(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(clonePVC("ns-dead-module-efivars", "ns-dead", corev1.ClaimPending, "")).
		Build()

	if err := newReaper(c).sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pvcExists(t, c, "ns-dead-module-efivars") {
		t.Fatal("orphaned Pending clone PVC was not deleted")
	}
}

// TestReaper_KeepsPVCOfLiveClone: on this cluster clone CRs are ephemeral, so a live
// VM carrying the clone label — not the clone CR — is the liveness signal. A Pending
// PVC whose clone still has a VM must be retained.
func TestReaper_KeepsPVCOfLiveClone(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(clonePVC("ns-live-module-efivars", "ns-live", corev1.ClaimPending, "")).
		Build()
	// A VM of clone ns-live still exists (references a different, bound disk).
	vm := vmReferencingPVC("ns-live-module", "ns-live-module-rootdisk")
	vm.SetLabels(map[string]string{cloneLabel: "ns-live"})
	if err := c.Create(context.Background(), vm); err != nil {
		t.Fatal(err)
	}

	if err := newReaper(c).sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pvcExists(t, c, "ns-live-module-efivars") {
		t.Fatal("Pending PVC of a clone with a live VM was wrongly deleted")
	}
}

// TestReaper_KeepsInProgressClonePVC protects a clone still being provisioned: its
// VirtualMachineClone CR exists (matched by Status.CloneID) but no VM has been created
// yet, so its Pending PVC must not be reaped.
func TestReaper_KeepsInProgressClonePVC(t *testing.T) {
	liveClone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-clone", Namespace: reaperNS},
		Status:     v1alpha1.VirtualMachineCloneStatus{CloneID: "ns-live"},
	}
	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(clonePVC("ns-live-module-efivars", "ns-live", corev1.ClaimPending, ""), liveClone).
		Build()

	if err := newReaper(c).sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pvcExists(t, c, "ns-live-module-efivars") {
		t.Fatal("in-progress clone PVC was wrongly deleted")
	}
}

// TestReaper_KeepsBoundOrphan: a Bound orphan may hold a real VM disk, so the reaper
// must NOT auto-delete it even when the clone is fully dead — it only surfaces via a
// metric. This prevents wholesale deletion of committed clone storage.
func TestReaper_KeepsBoundOrphan(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(clonePVC("ns-dead-server-rootdisk", "ns-dead", corev1.ClaimBound, "pv-ceph-37gi")).
		Build()

	if err := newReaper(c).sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pvcExists(t, c, "ns-dead-server-rootdisk") {
		t.Fatal("Bound orphan PVC was deleted; committed disk data must be preserved")
	}
}

// TestReaper_KeepsEFIVarsReferencedByHook guards the efivars-via-hook case: a Pending
// clone PVC that a VM references only through the hooks.kubevirt.io/hookSidecars
// annotation (not the volume list) must not be deleted.
func TestReaper_KeepsEFIVarsReferencedByHook(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(clonePVC("ns-dead-module-efivars", "ns-dead", corev1.ClaimPending, "")).
		Build()
	vm := vmReferencingPVC("ns-dead-module", "ns-dead-module-rootdisk")
	// EFI-vars PVC is attached via the hook sidecar annotation, not the volume list.
	_ = unstructured.SetNestedField(vm.Object,
		`[{"pvc":{"name":"ns-dead-module-efivars"}}]`,
		"spec", "template", "metadata", "annotations", "hooks.kubevirt.io/hookSidecars")
	if err := c.Create(context.Background(), vm); err != nil {
		t.Fatal(err)
	}

	if err := newReaper(c).sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pvcExists(t, c, "ns-dead-module-efivars") {
		t.Fatal("efivars PVC referenced via hook annotation was wrongly deleted")
	}
}

// TestReaper_ReapsOrphanedCloneVMNS deletes a clone's owner root once its PVCs are
// gone, but never touches a build-owned VirtualMachineNamespace.
func TestReaper_ReapsOrphanedCloneVMNS(t *testing.T) {
	cloneVMNS := &v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-dead", Namespace: reaperNS},
		Spec:       v1alpha1.VirtualMachineNamespaceSpec{OwnerRef: &v1alpha1.VirtualMachineNamespaceOwnerRef{Kind: "VirtualMachineClone", Name: "clone-x"}},
	}
	buildVMNS := &v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-build1", Namespace: reaperNS},
		Spec:       v1alpha1.VirtualMachineNamespaceSpec{OwnerRef: &v1alpha1.VirtualMachineNamespaceOwnerRef{Kind: "VirtualMachineBuild", Name: "build-x"}},
	}
	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(cloneVMNS, buildVMNS).
		Build()

	if err := newReaper(c).sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	vmnsExists := func(name string) bool {
		got := &v1alpha1.VirtualMachineNamespace{}
		return c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: reaperNS}, got) == nil
	}
	if vmnsExists("ns-dead") {
		t.Error("orphaned clone VirtualMachineNamespace was not deleted")
	}
	if !vmnsExists("vm-build1") {
		t.Error("build-owned VirtualMachineNamespace was wrongly deleted")
	}
}
