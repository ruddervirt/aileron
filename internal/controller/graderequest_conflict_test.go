// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

func kubevirtConflict(msg string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    http.StatusConflict,
		Reason:  metav1.StatusReasonConflict,
		Message: msg,
	}}
}

func addVMI(t *testing.T, r *GradeRequestReconciler, name, phase string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"})
	u.SetNamespace(gradeTargetNS)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, phase, "status", "phase")
	if _, err := r.DynamicClient.Resource(gradeKubevirtVMIGVR).Namespace(gradeTargetNS).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create VMI: %v", err)
	}
}

// The subresource client must decode KubeVirt's Status body so the reason
// survives into the error (and IsConflict works).
func TestKubevirtSubresourceActionDecodesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/namespaces/ns/virtualmachines/vm1/start") {
			t.Errorf("unexpected path %s", req.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict","code":409,"message":"VM is already running"}`))
	}))
	defer srv.Close()

	r := &GradeRequestReconciler{RESTConfig: &rest.Config{Host: srv.URL}}
	err := r.kubevirtSubresourceAction(context.Background(), "ns", "vm1", "start")
	if !apierrors.IsConflict(err) {
		t.Fatalf("want conflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "VM is already running") {
		t.Fatalf("reason not decoded: %v", err)
	}
	if !kubevirtStateConflict(err) {
		t.Fatalf("decoded state 409 should classify as state conflict: %v", err)
	}
}

func TestKubevirtStateConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"already running", kubevirtConflict("VM is already running"), true},
		{"not running", kubevirtConflict("VM is not running"), true},
		{"patch race", kubevirtConflict("jsonpatch test operation does not apply"), false},
		{"wrapped state", errors.Join(errors.New("stop vm"), kubevirtConflict("VM is not running")), true},
		{"server error", apierrors.NewInternalError(errors.New("boom")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kubevirtStateConflict(tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A user starting the VM between the phase read and our start call must not
// fail the grade: requeue without boot tracking, then follow the VMI phase.
func TestGradePowerOnConflictRequeues(t *testing.T) {
	t.Setenv("GRADE_MAX_CONCURRENT", "0")
	gr := offGradeRequest("grade-409", time.Now())
	r, _ := newGradeQueueReconciler(t, gr)
	r.powerOn = func(context.Context, string, string) error {
		return kubevirtConflict("VM is already running")
	}

	reconcileGrade(t, r, "grade-409")
	got := getGrade(t, r, "grade-409")
	vs := got.Status.VMStatuses[0]
	if vs.Phase == v1alpha1.GradeRequestPhaseFailed || strings.Contains(vs.Message, "failed to power on") {
		t.Fatalf("409 on start must not fail the grade: %+v", vs)
	}
	if vs.AutoStarted || vs.BootStartedAt != nil {
		t.Fatalf("409 on start must not stamp boot tracking: %+v", vs)
	}
	if got.Status.Phase != v1alpha1.GradeRequestPhasePending {
		t.Fatalf("request should stay Pending, got %s", got.Status.Phase)
	}

	// The user's boot comes up; the next pass sees a non-off VMI and the VM is
	// treated as one we did not boot.
	addVMI(t, r, "grade-409-vm", gradeVMIPhaseRunning)
	r.powerOn = func(context.Context, string, string) error {
		t.Fatal("must not start a VM that is already running")
		return nil
	}
	reconcileGrade(t, r, "grade-409")
	vs = getGrade(t, r, "grade-409").Status.VMStatuses[0]
	if vs.AutoStarted || vs.Phase == v1alpha1.GradeRequestPhaseFailed {
		t.Fatalf("running VM should proceed without AutoStarted: %+v", vs)
	}
}

// Past the boot window a persistent 409 with an off VMI fails, with the reason.
func TestGradePowerOnConflictBounded(t *testing.T) {
	t.Setenv("GRADE_MAX_CONCURRENT", "0")
	gr := offGradeRequest("grade-409-old", time.Now().Add(-gradeBootTimeout-time.Minute))
	r, _ := newGradeQueueReconciler(t, gr)
	r.powerOn = func(context.Context, string, string) error {
		return kubevirtConflict("VM is already running")
	}
	reconcileGrade(t, r, "grade-409-old")
	vs := getGrade(t, r, "grade-409-old").Status.VMStatuses[0]
	if vs.Phase != v1alpha1.GradeRequestPhaseFailed || !strings.Contains(vs.Message, "VM is already running") {
		t.Fatalf("want Failed with decoded reason, got %+v", vs)
	}
}

func TestPowerOffIfAutoStartedConflict(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		vmiPhase  string // "" = no VMI
		wantRet   bool
		wantPower bool
	}{
		{"state 409, VM off", kubevirtConflict("VM is not running"), "", true, true},
		{"state 409, VM still running", kubevirtConflict("VM is not running"), gradeVMIPhaseRunning, false, false},
		{"patch race, VM off", kubevirtConflict("jsonpatch test operation does not apply"), "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gr := offGradeRequest("grade-off", time.Now())
			r, _ := newGradeQueueReconciler(t, gr)
			if tc.vmiPhase != "" {
				addVMI(t, r, "grade-off-vm", tc.vmiPhase)
			}
			r.powerOff = func(context.Context, string, string) error { return tc.err }
			vs := &v1alpha1.GradeVMStatus{Name: "grade-off-vm", AutoStarted: true}
			if got := r.powerOffIfAutoStarted(context.Background(), gr, vs); got != tc.wantRet {
				t.Fatalf("return = %v, want %v", got, tc.wantRet)
			}
			if vs.PoweredOff != tc.wantPower {
				t.Fatalf("PoweredOff = %v, want %v", vs.PoweredOff, tc.wantPower)
			}
		})
	}
}
