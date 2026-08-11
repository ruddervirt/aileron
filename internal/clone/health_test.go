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

package clone

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	healthTestNS       = "team-a"
	healthTemplateNS   = "tmpl-ns"
	healthTemplateName = "ubuntu-base"
	healthBuildID      = "vm-build123"
)

// healthScheme registers everything CheckTemplateHealth's dependency chain
// (ValidateTemplate -> findBuild/ListTemplateVMs/templateOutputVolumesExist,
// plus the storage walk's own PVC/PV/VolumeSnapshot lookups) needs to run
// against a fake client.
func healthScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList"}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolumeList"}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})
	return s
}

func succeededBuild() *v1alpha1.VirtualMachineBuild {
	return &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: healthTemplateName, Namespace: healthTestNS},
		Status: v1alpha1.VirtualMachineBuildStatus{
			Phase:             v1alpha1.BuildPhaseSucceeded,
			TemplateNamespace: healthTemplateNS,
		},
	}
}

// healthTemplateVM builds a template VM referencing pvcName as its single
// rootdisk volume. When withEFI is true it also carries the hookSidecars
// annotation hasEFIVarsPVCMount looks for, naming
// "<buildID>-<vmShortName>-efivars" as the (separately checked) efivars PVC.
func healthTemplateVM(vmShortName, pvcName string, withEFI bool) *unstructured.Unstructured {
	vm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":      healthBuildID + "-" + vmShortName,
				"namespace": healthTemplateNS,
				"labels": map[string]any{
					LabelBuildID:              healthBuildID,
					LabelVMName:               vmShortName,
					"ruddervirt.io/build":     healthTemplateName,
					"ruddervirt.io/component": "template",
				},
			},
			"spec": map[string]any{
				"template": map[string]any{
					"metadata": map[string]any{"annotations": map[string]any{}},
					"spec": map[string]any{
						"volumes": []any{
							map[string]any{
								"name":                  "rootdisk",
								"persistentVolumeClaim": map[string]any{"claimName": pvcName},
							},
						},
					},
				},
			},
		},
	}
	if withEFI {
		hookJSON, _ := json.Marshal([]map[string]any{{
			"pvc": map[string]any{"name": healthBuildID + "-" + vmShortName + "-efivars"},
		}})
		_ = unstructured.SetNestedField(vm.Object, string(hookJSON), "spec", "template", "metadata", "annotations", "hooks.kubevirt.io/hookSidecars")
	}
	return vm
}

// boundPVC returns a Bound "out-module" PVC pointing at pvName — every test
// in this file uses the same template PVC name, so only the PV it binds to
// varies.
func boundPVC(pvName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "out-module", Namespace: healthTemplateNS},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func healthyPV() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-module"}}
}

func baseSnapshot(name, sourcePVC string, deleting bool) *unstructured.Unstructured {
	snap := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]any{
				"name":      name,
				"namespace": healthTemplateNS,
				"labels": map[string]any{
					LabelBaseSnapshot: "true",
					LabelSourcePVC:    sourcePVC,
				},
			},
		},
	}
	if deleting {
		// The fake client's tracker (like a real API server) refuses to seed
		// an object with a deletionTimestamp but no finalizer.
		snap.SetFinalizers([]string{"snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"})
		now := metav1.NewTime(time.Now())
		snap.SetDeletionTimestamp(&now)
	}
	return snap
}

func TestCheckTemplateHealth_Healthy(t *testing.T) {
	vm := healthTemplateVM("module", "out-module", false)
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm, boundPVC("pv-module"), healthyPV()).
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !health.Clonable {
		t.Errorf("Clonable = false, want true; missing=%v message=%q", health.Missing, health.Message)
	}
	if len(health.Missing) != 0 {
		t.Errorf("Missing = %v, want empty", health.Missing)
	}
	if health.Message != "" {
		t.Errorf("Message = %q, want empty", health.Message)
	}
}

func TestCheckTemplateHealth_NoBaseSnapshotYetIsHealthy(t *testing.T) {
	// Identical to the healthy case but named explicitly: a template with no
	// base snapshot yet (the normal pre-first-clone state) must not be
	// reported as unclonable.
	vm := healthTemplateVM("module", "out-module", false)
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm, boundPVC("pv-module"), healthyPV()).
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !health.Clonable {
		t.Errorf("Clonable = false, want true (no snapshot yet is healthy); missing=%v", health.Missing)
	}
}

func TestCheckTemplateHealth_TemplateVMsDeletedVolumesRemain(t *testing.T) {
	dv := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]any{
				"name":      "out-module",
				"namespace": healthTemplateNS,
				"labels":    map[string]any{"ruddervirt.io/build": healthTemplateName},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), dv).
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Clonable {
		t.Fatal("Clonable = true, want false (template VMs gone)")
	}
	want := []string{"template-vm/" + healthTemplateName}
	if !equalStringSlices(health.Missing, want) {
		t.Errorf("Missing = %v, want %v", health.Missing, want)
	}
	if !strings.Contains(health.Message, "must be rebuilt") {
		t.Errorf("Message = %q, want it to contain %q", health.Message, "must be rebuilt")
	}
	if !strings.Contains(health.Message, "output volumes remain") {
		t.Errorf("Message = %q, want the 'output volumes remain' diagnostic", health.Message)
	}
}

func TestCheckTemplateHealth_TemplateVMsAndVolumesGone(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild()).
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Clonable {
		t.Fatal("Clonable = true, want false")
	}
	want := []string{"template-vm/" + healthTemplateName}
	if !equalStringSlices(health.Missing, want) {
		t.Errorf("Missing = %v, want %v", health.Missing, want)
	}
	if !strings.Contains(health.Message, "fully removed") {
		t.Errorf("Message = %q, want the 'fully removed' diagnostic", health.Message)
	}
}

func TestCheckTemplateHealth_PVCMissing(t *testing.T) {
	vm := healthTemplateVM("module", "out-module", false)
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm). // no PVC created
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Clonable {
		t.Fatal("Clonable = true, want false (PVC missing)")
	}
	want := []string{"pvc/out-module"}
	if !equalStringSlices(health.Missing, want) {
		t.Errorf("Missing = %v, want %v", health.Missing, want)
	}
}

func TestCheckTemplateHealth_PVCNotBound(t *testing.T) {
	vm := healthTemplateVM("module", "out-module", false)
	pending := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "out-module", Namespace: healthTemplateNS},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm, pending).
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Clonable {
		t.Fatal("Clonable = true, want false (PVC not Bound)")
	}
	want := []string{"pvc/out-module"}
	if !equalStringSlices(health.Missing, want) {
		t.Errorf("Missing = %v, want %v", health.Missing, want)
	}
}

func TestCheckTemplateHealth_PVMissing(t *testing.T) {
	vm := healthTemplateVM("module", "out-module", false)
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm, boundPVC("pv-gone")). // no PV object
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Clonable {
		t.Fatal("Clonable = true, want false (PV missing)")
	}
	want := []string{"pv/pv-gone"}
	if !equalStringSlices(health.Missing, want) {
		t.Errorf("Missing = %v, want %v", health.Missing, want)
	}
}

func TestCheckTemplateHealth_EFIVarsPVCMissing(t *testing.T) {
	vm := healthTemplateVM("module", "out-module", true)
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm, boundPVC("pv-module"), healthyPV()). // no efivars PVC
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Clonable {
		t.Fatal("Clonable = true, want false (efivars PVC missing)")
	}
	wantEntry := "pvc/" + healthBuildID + "-module-efivars"
	want := []string{wantEntry}
	if !equalStringSlices(health.Missing, want) {
		t.Errorf("Missing = %v, want %v", health.Missing, want)
	}
}

func TestCheckTemplateHealth_BaseSnapshotBeingDeleted(t *testing.T) {
	vm := healthTemplateVM("module", "out-module", false)
	snap := baseSnapshot("out-module-clone1-snap", "out-module", true)
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm, boundPVC("pv-module"), healthyPV()).
		WithObjects(snap).
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Clonable {
		t.Fatal("Clonable = true, want false (base snapshot being deleted)")
	}
	want := []string{"volumesnapshot/out-module-clone1-snap"}
	if !equalStringSlices(health.Missing, want) {
		t.Errorf("Missing = %v, want %v", health.Missing, want)
	}
}

func TestCheckTemplateHealth_MultipleProblemsAllReported(t *testing.T) {
	vm1 := healthTemplateVM("module-a", "out-module-a", false) // PVC missing
	vm2 := healthTemplateVM("module-b", "out-module-b", false) // PVC not Bound
	pending := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "out-module-b", Namespace: healthTemplateNS},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	c := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm1, vm2, pending).
		Build()

	health, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Clonable {
		t.Fatal("Clonable = true, want false")
	}
	want := []string{"pvc/out-module-a", "pvc/out-module-b"}
	if !equalStringSlices(health.Missing, want) {
		t.Errorf("Missing = %v, want both entries present (order-independent): %v", health.Missing, want)
	}
	if health.Message == "" {
		t.Error("Message = empty, want the first problem's diagnosis")
	}
}

// TestCheckTemplateHealth_NeverCreatesAnything guards the core invariant:
// unlike EnsureBaseSnapshotReady, this read-only check must never create a
// VolumeSnapshot (or anything else) — a health check that manufactures the
// state it claims to observe would be worthless.
func TestCheckTemplateHealth_NeverCreatesAnything(t *testing.T) {
	vm := healthTemplateVM("module", "out-module", false)
	base := fake.NewClientBuilder().
		WithScheme(healthScheme(t)).
		WithObjects(succeededBuild(), vm, boundPVC("pv-module"), healthyPV()).
		Build()

	c := interceptedClient(base, func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		t.Fatalf("CheckTemplateHealth must not create %T %s, but tried to", obj, obj.GetName())
		return nil
	})

	if _, err := CheckTemplateHealth(context.Background(), c, healthTestNS, healthTemplateName); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func interceptedClient(base client.WithWatch, create func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error) client.WithWatch {
	return interceptor.NewClient(base, interceptor.Funcs{Create: create})
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	gotSorted := append([]string(nil), got...)
	wantSorted := append([]string(nil), want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			return false
		}
	}
	return true
}
