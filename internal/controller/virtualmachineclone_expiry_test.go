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
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func expiryScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolveExpiry_DefaultTTL(t *testing.T) {
	start := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	vmClone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a"},
		Status:     v1alpha1.VirtualMachineCloneStatus{StartTime: &start},
	}

	r := &VirtualMachineCloneReconciler{Client: fake.NewClientBuilder().WithScheme(expiryScheme(t)).Build()}
	r.resolveExpiry(context.Background(), vmClone)

	want := start.Add(defaultCloneTTL)
	if vmClone.Status.ExpiresAt == nil || !vmClone.Status.ExpiresAt.Time.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", vmClone.Status.ExpiresAt, want)
	}
}

func TestResolveExpiry_SpecTTLOverride(t *testing.T) {
	start := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ttl := metav1.Duration{Duration: 2 * time.Hour}
	vmClone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a"},
		Spec:       v1alpha1.VirtualMachineCloneSpec{TTL: &ttl},
		Status:     v1alpha1.VirtualMachineCloneStatus{StartTime: &start},
	}

	r := &VirtualMachineCloneReconciler{Client: fake.NewClientBuilder().WithScheme(expiryScheme(t)).Build()}
	r.resolveExpiry(context.Background(), vmClone)

	want := start.Add(2 * time.Hour)
	if vmClone.Status.ExpiresAt == nil || !vmClone.Status.ExpiresAt.Time.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", vmClone.Status.ExpiresAt, want)
	}
}

func TestResolveExpiry_ReconcilerDefaultTTL(t *testing.T) {
	start := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	vmClone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a"},
		Status:     v1alpha1.VirtualMachineCloneStatus{StartTime: &start},
	}

	r := &VirtualMachineCloneReconciler{
		Client:          fake.NewClientBuilder().WithScheme(expiryScheme(t)).Build(),
		DefaultCloneTTL: 3 * time.Hour,
	}
	r.resolveExpiry(context.Background(), vmClone)

	// Regression guard: without an explicit spec.ttl, the reconciler-configured
	// default must win over the built-in 720h fallback — this is the case that
	// would silently break if spec.ttl ever grew a +kubebuilder:default, since
	// the API server would populate it before the controller ever sees nil.
	want := start.Add(3 * time.Hour)
	if vmClone.Status.ExpiresAt == nil || !vmClone.Status.ExpiresAt.Time.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v (reconciler DefaultCloneTTL should apply)", vmClone.Status.ExpiresAt, want)
	}
}

func TestResolveExpiry_UsesAgeAnchorOverStartTime(t *testing.T) {
	start := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	anchor := metav1.NewTime(time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC))
	ttl := metav1.Duration{Duration: time.Hour}
	vmClone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a"},
		Spec:       v1alpha1.VirtualMachineCloneSpec{TTL: &ttl},
		Status: v1alpha1.VirtualMachineCloneStatus{
			StartTime: &start,
			AgeAnchor: &anchor,
		},
	}

	r := &VirtualMachineCloneReconciler{Client: fake.NewClientBuilder().WithScheme(expiryScheme(t)).Build()}
	r.resolveExpiry(context.Background(), vmClone)

	want := anchor.Add(time.Hour)
	if vmClone.Status.ExpiresAt == nil || !vmClone.Status.ExpiresAt.Time.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v (should anchor from AgeAnchor, not StartTime)", vmClone.Status.ExpiresAt, want)
	}
}

func TestResolveExpiry_InheritsPredecessorExpiresAtVerbatim(t *testing.T) {
	predecessorExpiry := metav1.NewTime(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	predecessor := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-prev", UID: "uid-prev"},
		Status: v1alpha1.VirtualMachineCloneStatus{
			CloneID:   "ns-prev",
			ExpiresAt: &predecessorExpiry,
		},
	}

	start := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	// Successor has its own, much shorter TTL — it must NOT be used, since
	// inheriting the predecessor's exact expiresAt is what keeps "delete at
	// the same wall-clock time as the predecessor" true even when TTLs differ.
	shortTTL := metav1.Duration{Duration: time.Minute}
	successor := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-next", UID: "uid-next"},
		Spec: v1alpha1.VirtualMachineCloneSpec{
			ReplacesCloneID: "ns-prev",
			TTL:             &shortTTL,
		},
		Status: v1alpha1.VirtualMachineCloneStatus{StartTime: &start},
	}

	c := fake.NewClientBuilder().WithScheme(expiryScheme(t)).WithObjects(predecessor, successor).Build()
	r := &VirtualMachineCloneReconciler{Client: c}
	r.resolveExpiry(context.Background(), successor)

	if successor.Status.ExpiresAt == nil || !successor.Status.ExpiresAt.Time.Equal(predecessorExpiry.Time) {
		t.Fatalf("ExpiresAt = %v, want inherited %v", successor.Status.ExpiresAt, predecessorExpiry.Time)
	}
}

func TestResolveExpiry_ReplacesCloneIDWithoutPredecessorExpiresAtFallsThrough(t *testing.T) {
	// Predecessor pre-dates this feature: it has a CloneID but no ExpiresAt.
	predecessor := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-prev", UID: "uid-prev"},
		Status:     v1alpha1.VirtualMachineCloneStatus{CloneID: "ns-prev"},
	}

	start := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ttl := metav1.Duration{Duration: time.Hour}
	successor := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-next", UID: "uid-next"},
		Spec: v1alpha1.VirtualMachineCloneSpec{
			ReplacesCloneID: "ns-prev",
			TTL:             &ttl,
		},
		Status: v1alpha1.VirtualMachineCloneStatus{StartTime: &start},
	}

	c := fake.NewClientBuilder().WithScheme(expiryScheme(t)).WithObjects(predecessor, successor).Build()
	r := &VirtualMachineCloneReconciler{Client: c}
	r.resolveExpiry(context.Background(), successor)

	want := start.Add(time.Hour)
	if successor.Status.ExpiresAt == nil || !successor.Status.ExpiresAt.Time.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want freshly computed %v", successor.Status.ExpiresAt, want)
	}
}
