// SPDX-License-Identifier: GPL-3.0-only

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The kind e2e cluster installs CRDs but no manager or virtualization stack.
// Drive the real reconciler against its API to exercise admission, initialization,
// validation, and persisted failure status end-to-end. Finalizer cleanup needs
// KubeOVN CRDs; on this minimal cluster teardown reclaims pending deletions.
func TestBuildMissingHTTPFileFailsPromptly(t *testing.T) {
	t.Setenv("OPERATOR_NAMESPACE", "default")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := &controller.VirtualMachineBuildReconciler{Client: kubeClient, Scheme: kubeClient.Scheme()}
	for _, phase := range []v1alpha1.BuildPhase{"", v1alpha1.BuildPhaseBuilding} {
		t.Run(string(phase), func(t *testing.T) {
			b := &v1alpha1.VirtualMachineBuild{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-missing-http-", Namespace: "default"},
				Spec: v1alpha1.VirtualMachineBuildSpec{
					HttpDirectory: &v1alpha1.HttpDirectory{Files: []v1alpha1.HttpDirectoryFileRef{{Name: "preseed.cfg"}}},
					VMs:           []v1alpha1.BuildVM{{Name: "one", Source: v1alpha1.BuildSource{Blank: true}}},
				},
			}
			if err := kubeClient.Create(ctx, b); err != nil {
				t.Fatal(err)
			}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cleanupCancel()
				if err := kubeClient.Delete(cleanupCtx, b); err != nil && !apierrors.IsNotFound(err) {
					t.Error(err)
				}
			})
			if phase != "" {
				// Simulate a CR already stuck in Building under an older controller.
				b.Status = v1alpha1.VirtualMachineBuildStatus{
					Phase: phase, BuildID: "vm-" + b.Name, BuildNamespace: "default",
					VMStatuses: []v1alpha1.VMBuildStatus{{Name: "one", Phase: v1alpha1.VMPhasePending}},
				}
				if err := kubeClient.Status().Update(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			for range 10 {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
				if err := kubeClient.Get(ctx, req.NamespacedName, b); err != nil {
					t.Fatal(err)
				}
				if b.Status.Phase == v1alpha1.BuildPhaseFailed {
					break
				}
			}
			want := `spec.httpDirectory.files[0]: file "preseed.cfg" not found in spec.files`
			if b.Status.Phase != v1alpha1.BuildPhaseFailed || !strings.Contains(b.Status.Message, want) {
				t.Fatalf("status = %+v; want Failed with %q", b.Status, want)
			}
			if b.Status.CompletionTime == nil {
				t.Fatal("failure has no completion time")
			}
			if len(b.Status.Conditions) == 0 || b.Status.Conditions[0].Reason != "BuildFailed" {
				t.Fatalf("missing failure condition: %+v", b.Status.Conditions)
			}
			opts := []client.ListOption{
				client.InNamespace("default"), client.MatchingLabels{"ruddervirt.io/build-id": b.Status.BuildID},
			}
			pods := &corev1.PodList{}
			if err := kubeClient.List(ctx, pods, opts...); err != nil {
				t.Fatal(err)
			}
			if len(pods.Items) != 0 {
				t.Fatalf("invalid manifest created %d pods", len(pods.Items))
			}
			vmns := &v1alpha1.VirtualMachineNamespaceList{}
			if err := kubeClient.List(ctx, vmns, opts...); err != nil {
				t.Fatal(err)
			}
			if len(vmns.Items) != 0 {
				t.Fatal("invalid manifest created a VirtualMachineNamespace")
			}
		})
	}
}
