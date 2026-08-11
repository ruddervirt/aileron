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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	"github.com/ruddervirt/aileron/internal/clone"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTemplateResourceToBuild(t *testing.T) {
	labeled := func(name, ns string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetName("some-resource")
		u.SetNamespace("tmpl-ns")
		labels := map[string]string{}
		if name != "" {
			labels[build.LabelBuild] = name
		}
		if ns != "" {
			labels[build.LabelBuildNamespace] = ns
		}
		u.SetLabels(labels)
		return u
	}

	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want []types.NamespacedName
	}{
		{
			name: "both labels present maps to the owning build",
			obj:  labeled("web-template", "team-a"),
			want: []types.NamespacedName{{Name: "web-template", Namespace: "team-a"}},
		},
		{
			name: "missing build label yields no request",
			obj:  labeled("", "team-a"),
			want: nil,
		},
		{
			name: "missing build-namespace label yields no request",
			obj:  labeled("web-template", ""),
			want: nil,
		},
		{
			name: "no labels at all yields no request",
			obj:  labeled("", ""),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := templateResourceToBuild(context.Background(), tt.obj)
			if len(got) != len(tt.want) {
				t.Fatalf("templateResourceToBuild() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i].NamespacedName != tt.want[i] {
					t.Errorf("request[%d] = %v, want %v", i, got[i].NamespacedName, tt.want[i])
				}
			}
		})
	}
}

const (
	thBuildName  = "web-template"
	thBuildNS    = "team-a"
	thTemplateNS = "tmpl-ns"
	thBuildID    = "vm-build123"
)

func templateHealthScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList"}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolumeList"}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})
	return s
}

func healthyTemplateVM() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]any{
				"name":      thBuildID + "-module",
				"namespace": thTemplateNS,
				"labels": map[string]any{
					"ruddervirt.io/build":     thBuildName,
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
								"persistentVolumeClaim": map[string]any{"claimName": "out-module"},
							},
						},
					},
				},
			},
		},
	}
}

func healthyTemplatePVCAndPV() (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "out-module", Namespace: thTemplateNS},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-module"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-module"}}
	return pvc, pv
}

func succeededBuildForHealthTest() *v1alpha1.VirtualMachineBuild {
	return &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: thBuildName, Namespace: thBuildNS},
		Status: v1alpha1.VirtualMachineBuildStatus{
			Phase:             v1alpha1.BuildPhaseSucceeded,
			TemplateNamespace: thTemplateNS,
		},
	}
}

func TestReconcileTemplateHealth_WritesOnFirstEvaluation(t *testing.T) {
	vmBuild := succeededBuildForHealthTest()
	vm := healthyTemplateVM()
	pvc, pv := healthyTemplatePVCAndPV()

	c := fake.NewClientBuilder().
		WithScheme(templateHealthScheme(t)).
		WithStatusSubresource(&v1alpha1.VirtualMachineBuild{}).
		WithObjects(vmBuild, vm, pvc, pv).
		Build()

	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}
	if _, err := r.reconcileTemplateHealth(context.Background(), vmBuild); err != nil {
		t.Fatalf("reconcileTemplateHealth: %v", err)
	}

	got := &v1alpha1.VirtualMachineBuild{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: thBuildName, Namespace: thBuildNS}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.TemplateHealth == nil {
		t.Fatal("Status.TemplateHealth = nil, want it set")
	}
	if !got.Status.TemplateHealth.Clonable {
		t.Errorf("Clonable = false, want true; missing=%v message=%q", got.Status.TemplateHealth.Missing, got.Status.TemplateHealth.Message)
	}
	cond := findCondition(got.Status.Conditions, v1alpha1.ConditionClonable)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Clonable condition = %+v, want status True", cond)
	}
}

func TestReconcileTemplateHealth_SkipsWriteWhenUnchanged(t *testing.T) {
	vmBuild := succeededBuildForHealthTest()
	vmBuild.Status.TemplateHealth = &v1alpha1.TemplateHealth{
		Clonable:  true,
		CheckedAt: metav1.NewTime(metav1.Now().Add(-time.Hour)),
	}
	vm := healthyTemplateVM()
	pvc, pv := healthyTemplatePVCAndPV()

	c := fake.NewClientBuilder().
		WithScheme(templateHealthScheme(t)).
		WithStatusSubresource(&v1alpha1.VirtualMachineBuild{}).
		WithObjects(vmBuild, vm, pvc, pv).
		Build()

	before := &v1alpha1.VirtualMachineBuild{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: thBuildName, Namespace: thBuildNS}, before); err != nil {
		t.Fatal(err)
	}

	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}
	if _, err := r.reconcileTemplateHealth(context.Background(), vmBuild); err != nil {
		t.Fatalf("reconcileTemplateHealth: %v", err)
	}

	after := &v1alpha1.VirtualMachineBuild{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: thBuildName, Namespace: thBuildNS}, after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("ResourceVersion changed (%s -> %s), want no status write for an unchanged verdict", before.ResourceVersion, after.ResourceVersion)
	}
	if !after.Status.TemplateHealth.CheckedAt.Equal(&before.Status.TemplateHealth.CheckedAt) {
		t.Errorf("CheckedAt changed, want it untouched since the write was skipped")
	}
}

func TestReconcileTemplateHealth_WritesOnRealChange(t *testing.T) {
	// A non-zero retention keeps this test focused on the status write, not
	// the retention-based deletion covered separately below — with the
	// default (unset) FAILURE_RETENTION, the build would be deleted in this
	// same reconcile the instant Clonable flips to False.
	t.Setenv("FAILURE_RETENTION", "1h")

	vmBuild := succeededBuildForHealthTest()
	vmBuild.Status.TemplateHealth = &v1alpha1.TemplateHealth{Clonable: true}
	// Template VM gone — the build's own object still reports (stale) Clonable: true.
	c := fake.NewClientBuilder().
		WithScheme(templateHealthScheme(t)).
		WithStatusSubresource(&v1alpha1.VirtualMachineBuild{}).
		WithObjects(vmBuild).
		Build()

	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}
	if _, err := r.reconcileTemplateHealth(context.Background(), vmBuild); err != nil {
		t.Fatalf("reconcileTemplateHealth: %v", err)
	}

	got := &v1alpha1.VirtualMachineBuild{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: thBuildName, Namespace: thBuildNS}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.TemplateHealth == nil || got.Status.TemplateHealth.Clonable {
		t.Fatalf("TemplateHealth = %+v, want Clonable: false", got.Status.TemplateHealth)
	}
	cond := findCondition(got.Status.Conditions, v1alpha1.ConditionClonable)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Clonable condition = %+v, want status False", cond)
	}
}

// buildWithClonableCondition returns a Succeeded build carrying a Clonable
// condition already set to the given status, transitioned howLongAgo in the
// past — the shape deleteIfUnclonableTooLong reads its verdict from.
func buildWithClonableCondition(status metav1.ConditionStatus, howLongAgo time.Duration) *v1alpha1.VirtualMachineBuild {
	b := succeededBuildForHealthTest()
	reason := "TemplateClonable"
	if status == metav1.ConditionFalse {
		reason = "TemplateUnclonable"
	}
	b.Status.Conditions = []metav1.Condition{{
		Type:               v1alpha1.ConditionClonable,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-howLongAgo)),
	}}
	return b
}

// TestDeleteIfUnclonableTooLong_AnchorsOnConditionNotCompletionTime guards the
// exact regression this function exists to avoid: a build can complete
// (Status.CompletionTime) months before its template ever goes unclonable —
// a template is typically reaped long after the build that produced it
// finished. Anchoring retention on CompletionTime instead of the Clonable
// condition's LastTransitionTime would make FAILURE_RETENTION nearly always
// negative in practice, deleting on the very first reconcile with no grace
// period at all. This build has a CompletionTime from 90 days ago but a
// Clonable transition from one minute ago; it must survive the full
// retention window.
func TestDeleteIfUnclonableTooLong_AnchorsOnConditionNotCompletionTime(t *testing.T) {
	t.Setenv("FAILURE_RETENTION", "10m")
	vmBuild := buildWithClonableCondition(metav1.ConditionFalse, time.Minute)
	oldCompletion := metav1.NewTime(time.Now().Add(-90 * 24 * time.Hour))
	vmBuild.Status.CompletionTime = &oldCompletion

	c := fake.NewClientBuilder().WithScheme(templateHealthScheme(t)).WithObjects(vmBuild).Build()
	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}

	deleted, err := r.deleteIfUnclonableTooLong(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("deleteIfUnclonableTooLong: %v", err)
	}
	if deleted {
		t.Fatal("deleted = true, want false — a 90-day-old CompletionTime must not shortcut the 10m retention window; only the Clonable transition time (1m ago) should count")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: thBuildName, Namespace: thBuildNS}, &v1alpha1.VirtualMachineBuild{}); err != nil {
		t.Fatalf("build should still exist: %v", err)
	}
}

// TestDeleteIfUnclonableTooLong_EmitsEventAndMetric guards the audit trail: a
// mass deletion (every pre-existing unclonable build flipping at once right
// after this feature ships) must be distinguishable from a user-initiated
// delete without grepping controller logs.
func TestDeleteIfUnclonableTooLong_EmitsEventAndMetric(t *testing.T) {
	t.Setenv("FAILURE_RETENTION", "10m")
	before := testutil.ToFloat64(buildsDeletedUnclonable)

	vmBuild := buildWithClonableCondition(metav1.ConditionFalse, time.Hour)
	c := fake.NewClientBuilder().WithScheme(templateHealthScheme(t)).WithObjects(vmBuild).Build()
	rec := events.NewFakeRecorder(1)
	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme(), Recorder: rec}

	deleted, err := r.deleteIfUnclonableTooLong(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("deleteIfUnclonableTooLong: %v", err)
	}
	if !deleted {
		t.Fatal("deleted = false, want true")
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "TemplateUnclonableCleanup") {
			t.Errorf("event = %q, want it to carry reason TemplateUnclonableCleanup", ev)
		}
	default:
		t.Error("no Event recorded, want one distinguishing this from a user-initiated delete")
	}

	if after := testutil.ToFloat64(buildsDeletedUnclonable); after != before+1 {
		t.Errorf("buildsDeletedUnclonable = %v, want %v", after, before+1)
	}
}

func TestDeleteIfUnclonableTooLong_DeletesPastRetention(t *testing.T) {
	t.Setenv("FAILURE_RETENTION", "10m")
	vmBuild := buildWithClonableCondition(metav1.ConditionFalse, time.Hour)
	c := fake.NewClientBuilder().WithScheme(templateHealthScheme(t)).WithObjects(vmBuild).Build()
	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}

	deleted, err := r.deleteIfUnclonableTooLong(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("deleteIfUnclonableTooLong: %v", err)
	}
	if !deleted {
		t.Fatal("deleted = false, want true (unclonable for 1h, retention 10m)")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: thBuildName, Namespace: thBuildNS}, &v1alpha1.VirtualMachineBuild{}); err == nil {
		t.Fatal("build still exists, want it deleted")
	}
}

func TestDeleteIfUnclonableTooLong_KeepsWithinRetention(t *testing.T) {
	t.Setenv("FAILURE_RETENTION", "10m")
	vmBuild := buildWithClonableCondition(metav1.ConditionFalse, time.Minute)
	c := fake.NewClientBuilder().WithScheme(templateHealthScheme(t)).WithObjects(vmBuild).Build()
	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}

	deleted, err := r.deleteIfUnclonableTooLong(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("deleteIfUnclonableTooLong: %v", err)
	}
	if deleted {
		t.Fatal("deleted = true, want false (unclonable for 1m, retention 10m)")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: thBuildName, Namespace: thBuildNS}, &v1alpha1.VirtualMachineBuild{}); err != nil {
		t.Fatalf("build should still exist: %v", err)
	}
}

func TestDeleteIfUnclonableTooLong_KeepsWhenClonable(t *testing.T) {
	t.Setenv("FAILURE_RETENTION", "0s")
	vmBuild := buildWithClonableCondition(metav1.ConditionTrue, 24*time.Hour)
	c := fake.NewClientBuilder().WithScheme(templateHealthScheme(t)).WithObjects(vmBuild).Build()
	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}

	deleted, err := r.deleteIfUnclonableTooLong(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("deleteIfUnclonableTooLong: %v", err)
	}
	if deleted {
		t.Fatal("deleted = true, want false (Clonable condition is True)")
	}
}

func TestDeleteIfUnclonableTooLong_SkipsInDebugMode(t *testing.T) {
	t.Setenv("FAILURE_RETENTION", "0s")
	t.Setenv("DEBUG", "true")
	vmBuild := buildWithClonableCondition(metav1.ConditionFalse, 24*time.Hour)
	c := fake.NewClientBuilder().WithScheme(templateHealthScheme(t)).WithObjects(vmBuild).Build()
	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}

	deleted, err := r.deleteIfUnclonableTooLong(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("deleteIfUnclonableTooLong: %v", err)
	}
	if deleted {
		t.Fatal("deleted = true, want false (DEBUG mode retains resources for inspection)")
	}
}

// TestReconcileTemplateHealth_DeletesOnSkippedWritePath guards the case
// where the verdict is unchanged (no status write this reconcile) but the
// build has still been unclonable past retention since an earlier reconcile
// — the retention check must run on the "current" object either way, not
// only right after a fresh write.
func TestReconcileTemplateHealth_DeletesOnSkippedWritePath(t *testing.T) {
	t.Setenv("FAILURE_RETENTION", "10m")

	vmBuild := succeededBuildForHealthTest()
	// No template VM/PVC exist in this fixture — the build is unclonable.
	c := fake.NewClientBuilder().
		WithScheme(templateHealthScheme(t)).
		WithStatusSubresource(&v1alpha1.VirtualMachineBuild{}).
		WithObjects(vmBuild).
		Build()

	// Compute the verdict directly and seed it as the build's already-
	// persisted status (as an earlier reconcile would have written it), with
	// a LastTransitionTime an hour in the past. Since nothing about the
	// fixture changes, reconcileTemplateHealth's own CheckTemplateHealth call
	// below reproduces an identical verdict, so the write is genuinely
	// skipped — this test exists to prove the retention check still runs on
	// that skip-write path, not only right after a fresh write.
	existing, err := clone.CheckTemplateHealth(context.Background(), c, thBuildNS, thBuildName)
	if err != nil {
		t.Fatalf("CheckTemplateHealth: %v", err)
	}
	if existing.Clonable {
		t.Fatal("fixture is supposed to be unclonable (no template VM/PVC)")
	}
	vmBuild.Status.TemplateHealth = &existing
	vmBuild.Status.Conditions = []metav1.Condition{{
		Type:               v1alpha1.ConditionClonable,
		Status:             metav1.ConditionFalse,
		Reason:             "TemplateUnclonable",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}
	if err := c.Status().Update(context.Background(), vmBuild); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}
	if _, err := r.reconcileTemplateHealth(context.Background(), vmBuild); err != nil {
		t.Fatalf("reconcileTemplateHealth: %v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: thBuildName, Namespace: thBuildNS}, &v1alpha1.VirtualMachineBuild{}); err == nil {
		t.Fatal("build still exists, want it deleted despite the status write being skipped")
	}
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
