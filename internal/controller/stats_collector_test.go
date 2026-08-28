// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStatsCollector_CollectBuildsClonesGradesVMNS(t *testing.T) {
	build1 := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns1"},
		Status:     v1alpha1.VirtualMachineBuildStatus{Phase: v1alpha1.BuildPhaseBuilding},
	}
	build2 := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "b2", Namespace: "ns1"},
		Status:     v1alpha1.VirtualMachineBuildStatus{Phase: v1alpha1.BuildPhaseBuilding},
	}
	build3 := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "b3", Namespace: "ns1"},
		Status:     v1alpha1.VirtualMachineBuildStatus{Phase: v1alpha1.BuildPhaseSucceeded},
	}
	clone1 := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns1"},
		Status:     v1alpha1.VirtualMachineCloneStatus{Phase: v1alpha1.ClonePhaseReady},
	}
	gr1 := &v1alpha1.GradeRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: reaperNS},
		Status: v1alpha1.GradeRequestStatus{
			Phase: v1alpha1.GradeRequestPhaseRunning, ActiveSlots: 3, MaxSlots: 10, QueuedCount: 2,
		},
	}
	gr2 := &v1alpha1.GradeRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "g2", Namespace: reaperNS},
		Status: v1alpha1.GradeRequestStatus{
			// Terminal — must not contribute to the concurrency gauges.
			Phase: v1alpha1.GradeRequestPhaseReady, ActiveSlots: 99, MaxSlots: 99, QueuedCount: 99,
		},
	}
	vmns1 := &v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: "vmns1", Namespace: reaperNS},
		Status:     v1alpha1.VirtualMachineNamespaceStatus{Phase: v1alpha1.VMNamespacePhaseActive},
	}

	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(build1, build2, build3, clone1, gr1, gr2, vmns1).
		Build()

	sc := &StatsCollector{Client: c, Reader: c}
	if err := sc.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if got := testutil.ToFloat64(buildCount.WithLabelValues(string(v1alpha1.BuildPhaseBuilding))); got != 2 {
		t.Errorf("buildCount{phase=Building} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(buildCount.WithLabelValues(string(v1alpha1.BuildPhaseSucceeded))); got != 1 {
		t.Errorf("buildCount{phase=Succeeded} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(cloneCount.WithLabelValues(string(v1alpha1.ClonePhaseReady))); got != 1 {
		t.Errorf("cloneCount{phase=Ready} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(gradeReqCount.WithLabelValues(string(v1alpha1.GradeRequestPhaseRunning))); got != 1 {
		t.Errorf("gradeReqCount{phase=Running} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(vmnsCount.WithLabelValues(string(v1alpha1.VMNamespacePhaseActive))); got != 1 {
		t.Errorf("vmnsCount{phase=Active} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(gradeReqActiveSlots); got != 3 {
		t.Errorf("gradeReqActiveSlots = %v, want 3 (the terminal GradeRequest must be excluded)", got)
	}
	if got := testutil.ToFloat64(gradeReqMaxSlots); got != 10 {
		t.Errorf("gradeReqMaxSlots = %v, want 10", got)
	}
	if got := testutil.ToFloat64(gradeReqQueued); got != 2 {
		t.Errorf("gradeReqQueued = %v, want 2", got)
	}
}

func TestStatsCollector_CollectPVCBytes(t *testing.T) {
	storage := func(gb int64) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(gb<<30, resource.BinarySI)}
	}
	buildPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "build-pvc", Namespace: reaperNS, Labels: map[string]string{build.LabelBuildID: "b1"}},
		Spec:       corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: storage(10)}},
		Status:     corev1.PersistentVolumeClaimStatus{Capacity: storage(10)},
	}
	clonePVCObj := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-pvc", Namespace: reaperNS, Labels: map[string]string{cloneLabel: "c1"}},
		Spec:       corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: storage(5)}},
		Status:     corev1.PersistentVolumeClaimStatus{Capacity: storage(5)},
	}
	unrelatedPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "other-pvc", Namespace: reaperNS},
		Spec:       corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: storage(999)}},
	}

	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(buildPVC, clonePVCObj, unrelatedPVC).
		Build()

	sc := &StatsCollector{Client: c, Reader: c}
	if err := sc.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	want := float64(10 << 30)
	if got := testutil.ToFloat64(pvcBytes.WithLabelValues("requested", "build")); got != want {
		t.Errorf("pvcBytes{type=requested,owner=build} = %v, want %v", got, want)
	}
	want = float64(5 << 30)
	if got := testutil.ToFloat64(pvcBytes.WithLabelValues("requested", "clone")); got != want {
		t.Errorf("pvcBytes{type=requested,owner=clone} = %v, want %v", got, want)
	}
}

func TestStatsCollector_CollectPodErrors(t *testing.T) {
	crashPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "crash", Namespace: reaperNS, Labels: map[string]string{build.LabelBuildID: "b1"}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
	oomPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "oom", Namespace: reaperNS, Labels: map[string]string{cloneLabel: "c1"}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
			}},
		},
	}
	healthyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "healthy", Namespace: reaperNS, Labels: map[string]string{build.LabelBuildID: "b1"}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(reaperScheme(t)).
		WithObjects(crashPod, oomPod, healthyPod).
		Build()

	sc := &StatsCollector{Client: c, Reader: c}
	if err := sc.collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if got := testutil.ToFloat64(podErrors.WithLabelValues("CrashLoopBackOff")); got != 1 {
		t.Errorf("podErrors{reason=CrashLoopBackOff} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(podErrors.WithLabelValues("OOMKilled")); got != 1 {
		t.Errorf("podErrors{reason=OOMKilled} = %v, want 1", got)
	}
}
