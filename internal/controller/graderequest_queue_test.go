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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

const (
	gradeOperatorNS = "ruddervirt-system"
	gradeTargetNS   = "target-ns"
)

// countingReader wraps an uncached reader and counts List calls so tests can
// assert the concurrency gate short-circuits (no list) when unlimited.
type countingReader struct {
	client.Reader
	lists int
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Reader.List(ctx, list, opts...)
}

// offGradeRequest builds a GradeRequest for a single powered-off VM. Its target
// VM is named "<name>-vm" and lives in gradeTargetNS.
func offGradeRequest(name string, created time.Time) *v1alpha1.GradeRequest {
	return &v1alpha1.GradeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         gradeOperatorNS,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: v1alpha1.GradeRequestSpec{
			Namespace: gradeTargetNS,
			VMs: []v1alpha1.GradeVM{{
				Name:     name + "-vm",
				Commands: []string{"echo hi"},
				User:     "u",
				Password: "p",
			}},
		},
	}
}

// vmObject is a KubeVirt VirtualMachine carrying the OS label the grader reads
// to resolve the grade method.
func vmObject(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"})
	u.SetNamespace(gradeTargetNS)
	u.SetName(name)
	u.SetLabels(map[string]string{"ruddervirt.io/os": "linux"})
	return u
}

// newGradeQueueReconciler wires a reconciler over fake clients. grs are seeded
// into the controller-runtime client; a VM object is registered in the dynamic
// client for every gradable VM so resolveGradeMethod succeeds. No VMI is
// registered, so every target VM reads as powered off. powerOn is stubbed to
// record the VMs booted this run.
func newGradeQueueReconciler(t *testing.T, grs ...*v1alpha1.GradeRequest) (*GradeRequestReconciler, *[]string) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	objs := make([]client.Object, 0, len(grs))
	var vmObjs []runtime.Object
	for _, g := range grs {
		objs = append(objs, g)
		for _, vm := range g.Spec.VMs {
			vmObjs = append(vmObjs, vmObject(vm.Name))
		}
	}

	c := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.GradeRequest{}).
		WithObjects(objs...).
		Build()

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			gradeKubevirtVMGVR:  "VirtualMachineList",
			gradeKubevirtVMIGVR: "VirtualMachineInstanceList",
		}, vmObjs...)

	powered := &[]string{}
	r := &GradeRequestReconciler{
		Client:        c,
		Reader:        c,
		Scheme:        scheme,
		DynamicClient: dyn,
		Clientset:     k8sfake.NewClientset(),
		powerOn: func(_ context.Context, _, vmName string) error {
			*powered = append(*powered, vmName)
			return nil
		},
		powerOff: func(_ context.Context, _, _ string) error { return nil },
	}
	return r, powered
}

func reconcileGrade(t *testing.T, r *GradeRequestReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: gradeOperatorNS},
	}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

func getGrade(t *testing.T, r *GradeRequestReconciler, name string) *v1alpha1.GradeRequest {
	t.Helper()
	got := &v1alpha1.GradeRequest{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: gradeOperatorNS}, got); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return got
}

func vmBooted(gr *v1alpha1.GradeRequest) bool {
	return len(gr.Status.VMStatuses) == 1 && gr.Status.VMStatuses[0].BootStartedAt != nil
}

func vmQueued(gr *v1alpha1.GradeRequest) bool {
	return len(gr.Status.VMStatuses) == 1 && gr.Status.VMStatuses[0].QueuePosition != nil
}

func TestGradeMaxConcurrent(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want int32
	}{
		{"unset falls back to default", "", false, gradeDefaultMaxConcurrent},
		{"explicit zero is unlimited", "0", true, 0},
		{"positive cap", "5", true, 5},
		{"garbage falls back to default", "not-a-number", true, gradeDefaultMaxConcurrent},
		{"negative falls back to default", "-3", true, gradeDefaultMaxConcurrent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("GRADE_MAX_CONCURRENT", tc.env)
			} else {
				t.Setenv("GRADE_MAX_CONCURRENT", "")
			}
			if got := gradeMaxConcurrent(); got != tc.want {
				t.Fatalf("gradeMaxConcurrent() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestGradeConcurrencyCapEnforced: three powered-off VMs, limit 2 — exactly two
// boot and the newest queues.
func TestGradeConcurrencyCapEnforced(t *testing.T) {
	t.Setenv("GRADE_MAX_CONCURRENT", "2")
	base := time.Now().Add(-time.Hour)
	gr1 := offGradeRequest("grade-a", base)
	gr2 := offGradeRequest("grade-b", base.Add(time.Minute))
	gr3 := offGradeRequest("grade-c", base.Add(2*time.Minute))

	r, powered := newGradeQueueReconciler(t, gr1, gr2, gr3)
	reconcileGrade(t, r, "grade-a")
	reconcileGrade(t, r, "grade-b")
	reconcileGrade(t, r, "grade-c")

	booted, queued := 0, 0
	for _, name := range []string{"grade-a", "grade-b", "grade-c"} {
		g := getGrade(t, r, name)
		if vmBooted(g) {
			booted++
		}
		if vmQueued(g) {
			queued++
		}
	}
	if booted != 2 {
		t.Fatalf("booted = %d, want 2", booted)
	}
	if queued != 1 {
		t.Fatalf("queued = %d, want 1", queued)
	}
	if len(*powered) != 2 {
		t.Fatalf("powered on %d VMs (%v), want 2", len(*powered), *powered)
	}
	// FIFO: the newest request is the one left queued.
	if !vmQueued(getGrade(t, r, "grade-c")) {
		t.Fatalf("expected newest request grade-c to be queued, got %+v", getGrade(t, r, "grade-c").Status.VMStatuses)
	}
}

// TestGradeFIFOAcrossRequests: reconciling the newer request first must not let
// it jump the older one when only one slot is free.
func TestGradeFIFOAcrossRequests(t *testing.T) {
	t.Setenv("GRADE_MAX_CONCURRENT", "1")
	base := time.Now().Add(-time.Hour)
	older := offGradeRequest("grade-old", base)
	newer := offGradeRequest("grade-new", base.Add(time.Minute))

	r, _ := newGradeQueueReconciler(t, older, newer)

	// Newest reconciles first; it must yield the single slot to the older one.
	reconcileGrade(t, r, "grade-new")
	if got := getGrade(t, r, "grade-new"); !vmQueued(got) {
		t.Fatalf("newer request should be queued behind the older one, got %+v", got.Status.VMStatuses)
	}

	reconcileGrade(t, r, "grade-old")
	if got := getGrade(t, r, "grade-old"); !vmBooted(got) {
		t.Fatalf("older request should be admitted, got %+v", got.Status.VMStatuses)
	}
}

// TestGradeBootWaitingCountsTowardCap: a VM already booting (no job yet) holds a
// slot, so a second request queues behind it.
func TestGradeBootWaitingCountsTowardCap(t *testing.T) {
	t.Setenv("GRADE_MAX_CONCURRENT", "1")
	base := time.Now().Add(-time.Hour)

	booting := offGradeRequest("grade-booting", base)
	now := metav1.Now()
	booting.Status.Phase = v1alpha1.GradeRequestPhasePending
	booting.Status.VMStatuses = []v1alpha1.GradeVMStatus{{
		Name:          "grade-booting-vm",
		Phase:         v1alpha1.GradeRequestPhasePending,
		AutoStarted:   true,
		BootStartedAt: &now,
	}}

	waiting := offGradeRequest("grade-waiting", base.Add(time.Minute))

	r, powered := newGradeQueueReconciler(t, booting, waiting)
	reconcileGrade(t, r, "grade-waiting")

	if got := getGrade(t, r, "grade-waiting"); !vmQueued(got) {
		t.Fatalf("request should queue behind the booting VM, got %+v", got.Status.VMStatuses)
	}
	if len(*powered) != 0 {
		t.Fatalf("no VM should have been powered on, got %v", *powered)
	}
}

// TestGradeQueuedStatusFields: a queued VM records a 1-based position, a helpful
// message, and the request surfaces the slot accounting.
func TestGradeQueuedStatusFields(t *testing.T) {
	t.Setenv("GRADE_MAX_CONCURRENT", "1")
	base := time.Now().Add(-time.Hour)
	gr1 := offGradeRequest("grade-a", base)
	gr2 := offGradeRequest("grade-b", base.Add(time.Minute))

	r, _ := newGradeQueueReconciler(t, gr1, gr2)
	reconcileGrade(t, r, "grade-a") // takes the only slot
	reconcileGrade(t, r, "grade-b") // queued

	g := getGrade(t, r, "grade-b")
	vs := g.Status.VMStatuses[0]
	if vs.QueuePosition == nil || *vs.QueuePosition < 1 {
		t.Fatalf("QueuePosition = %v, want >= 1", vs.QueuePosition)
	}
	if !strings.Contains(vs.Message, "Queued") {
		t.Fatalf("VM message = %q, want it to mention Queued", vs.Message)
	}
	if g.Status.MaxSlots != 1 {
		t.Fatalf("MaxSlots = %d, want 1", g.Status.MaxSlots)
	}
	if g.Status.QueuedCount != 1 {
		t.Fatalf("QueuedCount = %d, want 1", g.Status.QueuedCount)
	}
	if g.Status.Phase != v1alpha1.GradeRequestPhasePending {
		t.Fatalf("Phase = %q, want Pending while queued", g.Status.Phase)
	}
}

// TestGradeTerminalRequestsDoNotBlockQueue is a regression test for the queue
// wedge: a request that failed before its per-VM statuses were initialized
// (status.phase=Failed, status.vmStatuses=nil — e.g. failGradeRequest firing
// because the target VM no longer exists) must not count toward the grading
// queue. Before the fix, the admission gate classified every spec VM lacking a
// matching status entry as "waiting", so a pile of such orphans pushed live
// requests past the concurrency limit and nothing ever admitted despite free
// slots.
func TestGradeTerminalRequestsDoNotBlockQueue(t *testing.T) {
	t.Setenv("GRADE_MAX_CONCURRENT", "1")
	base := time.Now().Add(-time.Hour)

	// Three orphaned Failed requests, all created before the live one and all
	// with nil VMStatuses — the reprovision-orphan shape.
	orphans := make([]*v1alpha1.GradeRequest, 3)
	for i := range orphans {
		g := offGradeRequest("grade-orphan-"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Minute))
		g.Status.Phase = v1alpha1.GradeRequestPhaseFailed
		g.Status.VMStatuses = nil
		orphans[i] = g
	}
	live := offGradeRequest("grade-live", base.Add(time.Hour))

	r, powered := newGradeQueueReconciler(t, append(orphans, live)...)
	reconcileGrade(t, r, "grade-live")

	got := getGrade(t, r, "grade-live")
	if vmQueued(got) {
		t.Fatalf("live request must not queue behind terminal orphans, got %+v", got.Status.VMStatuses)
	}
	if !vmBooted(got) {
		t.Fatalf("live request should have been admitted and booted, got %+v", got.Status.VMStatuses)
	}
	if len(*powered) != 1 {
		t.Fatalf("powered on %d VMs (%v), want 1", len(*powered), *powered)
	}
}

// TestGradeUnlimitedSkipsGate: with no limit set, every VM boots immediately and
// the gate never lists (no uncached read).
func TestGradeUnlimitedSkipsGate(t *testing.T) {
	t.Setenv("GRADE_MAX_CONCURRENT", "0")
	base := time.Now().Add(-time.Hour)
	gr1 := offGradeRequest("grade-a", base)
	gr2 := offGradeRequest("grade-b", base.Add(time.Minute))
	gr3 := offGradeRequest("grade-c", base.Add(2*time.Minute))

	r, powered := newGradeQueueReconciler(t, gr1, gr2, gr3)
	cr := &countingReader{Reader: r.Reader}
	r.Reader = cr

	for _, name := range []string{"grade-a", "grade-b", "grade-c"} {
		reconcileGrade(t, r, name)
	}

	if len(*powered) != 3 {
		t.Fatalf("powered on %d VMs, want 3 (all admitted when unlimited)", len(*powered))
	}
	for _, name := range []string{"grade-a", "grade-b", "grade-c"} {
		if vmQueued(getGrade(t, r, name)) {
			t.Fatalf("%s should not be queued when unlimited", name)
		}
	}
	if cr.lists != 0 {
		t.Fatalf("admission gate listed %d times, want 0 when unlimited", cr.lists)
	}
}
