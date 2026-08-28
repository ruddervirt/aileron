// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func phaseMetricsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestFailBuild_IncrementsPhaseTransitionMetric guards the buildPhaseTransitions
// hook in failBuild, the centralized failure path for VirtualMachineBuild.
func TestFailBuild_IncrementsPhaseTransitionMetric(t *testing.T) {
	vmBuild := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns1"},
		Status:     v1alpha1.VirtualMachineBuildStatus{Phase: v1alpha1.BuildPhaseBuilding},
	}
	c := fake.NewClientBuilder().
		WithScheme(phaseMetricsScheme(t)).
		WithStatusSubresource(&v1alpha1.VirtualMachineBuild{}).
		WithObjects(vmBuild).
		Build()
	r := &VirtualMachineBuildReconciler{Client: c, Scheme: c.Scheme()}

	before := testutil.ToFloat64(buildPhaseTransitions.WithLabelValues(string(v1alpha1.BuildPhaseFailed)))
	if _, err := r.failBuild(context.Background(), vmBuild, "boom"); err != nil {
		t.Fatalf("failBuild: %v", err)
	}
	if after := testutil.ToFloat64(buildPhaseTransitions.WithLabelValues(string(v1alpha1.BuildPhaseFailed))); after != before+1 {
		t.Errorf("buildPhaseTransitions{phase=Failed} = %v, want %v", after, before+1)
	}
}

// TestFailClone_IncrementsPhaseTransitionMetric guards the clonePhaseTransitions
// hook in failClone, the centralized failure path for VirtualMachineClone.
func TestFailClone_IncrementsPhaseTransitionMetric(t *testing.T) {
	vmClone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns1"},
		Status:     v1alpha1.VirtualMachineCloneStatus{Phase: v1alpha1.ClonePhaseVMProvisioning},
	}
	c := fake.NewClientBuilder().
		WithScheme(phaseMetricsScheme(t)).
		WithStatusSubresource(&v1alpha1.VirtualMachineClone{}).
		WithObjects(vmClone).
		Build()
	r := &VirtualMachineCloneReconciler{Client: c, Scheme: c.Scheme()}

	before := testutil.ToFloat64(clonePhaseTransitions.WithLabelValues(string(v1alpha1.ClonePhaseFailed)))
	if _, err := r.failClone(context.Background(), vmClone, errors.New("boom")); err != nil {
		t.Fatalf("failClone: %v", err)
	}
	if after := testutil.ToFloat64(clonePhaseTransitions.WithLabelValues(string(v1alpha1.ClonePhaseFailed))); after != before+1 {
		t.Errorf("clonePhaseTransitions{phase=Failed} = %v, want %v", after, before+1)
	}
}

// TestGradeRequest_PendingPhase_IncrementsPhaseTransitionMetric guards the
// gradeReqPhaseTransitions hook in patchStatus, using the same reconcile
// harness as the concurrency-gate tests in graderequest_queue_test.go.
func TestGradeRequest_PendingPhase_IncrementsPhaseTransitionMetric(t *testing.T) {
	gr := offGradeRequest("grade-metric", metav1.Now().Time)
	r, _ := newGradeQueueReconciler(t, gr)

	before := testutil.ToFloat64(gradeReqPhaseTransitions.WithLabelValues(string(v1alpha1.GradeRequestPhasePending)))
	reconcileGrade(t, r, "grade-metric")

	got := getGrade(t, r, "grade-metric")
	if got.Status.Phase != v1alpha1.GradeRequestPhasePending {
		t.Fatalf("Status.Phase = %v, want %v", got.Status.Phase, v1alpha1.GradeRequestPhasePending)
	}
	if after := testutil.ToFloat64(gradeReqPhaseTransitions.WithLabelValues(string(v1alpha1.GradeRequestPhasePending))); after != before+1 {
		t.Errorf("gradeReqPhaseTransitions{phase=Pending} = %v, want %v", after, before+1)
	}
}
