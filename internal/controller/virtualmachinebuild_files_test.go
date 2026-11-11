// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestFileValidationBeforeResources(t *testing.T) {
	b := &v1alpha1.VirtualMachineBuild{Spec: v1alpha1.VirtualMachineBuildSpec{
		HttpDirectory: &v1alpha1.HttpDirectory{Files: []v1alpha1.HttpDirectoryFileRef{{Name: "preseed.cfg"}}},
		VMs:           []v1alpha1.BuildVM{{Name: "one", Source: v1alpha1.BuildSource{Blank: true}}},
	}, Status: v1alpha1.VirtualMachineBuildStatus{BuildID: "vm-test", VirtualMachineNamespace: "vm-test"}}
	// Nil clients intentionally panic if either handler touches resources before validation.
	for _, handler := range []build.PhaseHandler{&pendingHandler{}, &buildingHandler{}} {
		phase, err := handler.Handle(context.Background(), b.DeepCopy())
		if phase != v1alpha1.BuildPhaseFailed || err == nil || !strings.Contains(err.Error(), `file "preseed.cfg" not found in spec.files`) {
			t.Fatalf("handler %T = %s, %v; want Failed with missing file", handler, phase, err)
		}
	}
}

func TestReconcileRelayWaitMessage(t *testing.T) {
	t.Setenv("OPERATOR_NAMESPACE", "default")
	ctx := context.Background()
	b := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "relay-wait", Namespace: "default", Finalizers: []string{finalizerName}},
		Spec:       v1alpha1.VirtualMachineBuildSpec{VMs: []v1alpha1.BuildVM{{Name: "one", Source: v1alpha1.BuildSource{Blank: true}}}},
		Status:     v1alpha1.VirtualMachineBuildStatus{Phase: v1alpha1.BuildPhaseBuilding, BuildID: "vm-relay-wait", BuildNamespace: "default", VMStatuses: []v1alpha1.VMBuildStatus{{Name: "one", Phase: v1alpha1.VMPhasePending}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-relay-wait-relay", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "relay", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull failed"}}}}},
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(b, pod).WithObjects(b, pod).Build()
	r := &VirtualMachineBuildReconciler{Client: cl, Scheme: cl.Scheme()}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	got := &v1alpha1.VirtualMachineBuild{}
	if err := cl.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Status.Message, "ImagePullBackOff: pull failed") {
		t.Fatalf("wait message not persisted: %q", got.Status.Message)
	}
	if got.Status.Phase != v1alpha1.BuildPhaseBuilding {
		t.Fatalf("phase = %s", got.Status.Phase)
	}

	if err := cl.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	if err := cl.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Message != "" {
		t.Fatalf("stale message not cleared: %q", got.Status.Message)
	}
	if got.Status.VMStatuses[0].Phase != v1alpha1.VMPhaseSourceImporting {
		t.Fatalf("VM did not advance: %s", got.Status.VMStatuses[0].Phase)
	}
}
