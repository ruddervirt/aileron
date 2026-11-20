// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"context"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func provisioningClone() *v1alpha1.VirtualMachineClone {
	return &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a", Namespace: "tenant"},
		Status: v1alpha1.VirtualMachineCloneStatus{
			Phase:          v1alpha1.ClonePhaseVolumeProvisioning,
			CloneID:        "ns-abc",
			CloneNamespace: OperatorNamespace(),
			VolumeStates:   []v1alpha1.CloneVolumeStatus{{VolumeName: "root"}},
		},
	}
}

func vmnsTestScheme(t *testing.T) client.WithWatch {
	t.Helper()
	s := expiryScheme(t)
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).Build()
}

func assertNoClonePVCs(t *testing.T, c client.Client) {
	t.Helper()
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcs); err != nil {
		t.Fatal(err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("expected no PVCs to be created without an owning VirtualMachineNamespace, got %d", len(pvcs.Items))
	}
}

// A VirtualMachineNamespace deleted out from under an in-progress clone must be
// recreated before any PVC is created, so the PVCs are never left unowned.
func TestVolumeProvisioning_RecreatesMissingVMNS(t *testing.T) {
	c := vmnsTestScheme(t)
	vmClone := provisioningClone()
	r := &VirtualMachineCloneReconciler{Client: c}

	res, err := r.handleVolumeProvisioning(context.Background(), vmClone)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue after recreating the VirtualMachineNamespace")
	}

	got := &v1alpha1.VirtualMachineNamespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ns-abc", Namespace: OperatorNamespace()}, got); err != nil {
		t.Fatalf("VirtualMachineNamespace was not recreated: %v", err)
	}
	if ref := got.Spec.OwnerRef; ref == nil || ref.Kind != "VirtualMachineClone" || ref.Name != "clone-a" || ref.Namespace != "tenant" {
		t.Errorf("recreated VirtualMachineNamespace has wrong ownerRef: %+v", got.Spec.OwnerRef)
	}
	if got.Labels[cloneLabel] != "ns-abc" {
		t.Errorf("recreated VirtualMachineNamespace missing clone label: %v", got.Labels)
	}
	assertNoClonePVCs(t, c)
}

// The owner lookup must use the uncached reader: a cache still holding a deleted
// record would yield a dead UID, and GC deletes any PVC owned by it.
func TestVolumeProvisioning_TrustsAPIReaderOverStaleCache(t *testing.T) {
	cached := vmnsTestScheme(t)
	vmClone := provisioningClone()
	stale := cloneVMNS(vmClone)
	stale.UID = "dead-uid"
	if err := cached.Create(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	live := vmnsTestScheme(t) // API server: record is gone

	r := &VirtualMachineCloneReconciler{Client: cached, APIReader: live}
	res, err := r.handleVolumeProvisioning(context.Background(), vmClone)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue when the live reader reports the VirtualMachineNamespace missing")
	}
	assertNoClonePVCs(t, cached)
}
