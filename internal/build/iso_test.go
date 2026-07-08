package build

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func isoScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(dvGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: dvGVK.Group, Version: dvGVK.Version, Kind: dvGVK.Kind + "List",
	}, &unstructured.UnstructuredList{})
	return s
}

// cacheDV builds an iso-cache DataVolume in the given import state. runningCond
// is a (status, reason) pair for the Running condition; nil omits conditions.
func cacheDV(name string, lastUsed time.Time, phase string, runningCond []string) *unstructured.Unstructured {
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(dvGVK)
	dv.SetName(name)
	dv.SetNamespace("op-ns")
	dv.SetLabels(map[string]string{"ruddervirt.io/iso-cache": "true"})
	dv.SetAnnotations(map[string]string{
		"ruddervirt.io/iso-last-used": lastUsed.UTC().Format(time.RFC3339),
		"ruddervirt.io/iso-url":       "https://example.test/some.iso",
	})
	status := map[string]any{"phase": phase}
	if runningCond != nil {
		status["conditions"] = []any{
			map[string]any{"type": "Bound", "status": "False", "reason": "Pending"},
			map[string]any{"type": "Running", "status": runningCond[0], "reason": runningCond[1]},
		}
	}
	dv.Object["status"] = status
	return dv
}

func TestCleanupExpiredISOs(t *testing.T) {
	expired := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-1 * time.Hour)

	tests := []struct {
		name    string
		dv      *unstructured.Unstructured
		deleted bool
	}{
		{
			name:    "expired succeeded import → reaped",
			dv:      cacheDV("iso-a", expired, PhaseSucceeded, nil),
			deleted: true,
		},
		{
			name:    "expired failed import → reaped",
			dv:      cacheDV("iso-b", expired, PhaseFailed, nil),
			deleted: true,
		},
		{
			// The importer-prime crashloop case: a dead source URL keeps the
			// DV in a non-terminal phase forever while CDI retries.
			name:    "expired stalled import (Running=False/Error) → reaped",
			dv:      cacheDV("iso-c", expired, "ImportInProgress", []string{"False", "Error"}),
			deleted: true,
		},
		{
			name:    "expired but actively downloading (Running=True) → kept",
			dv:      cacheDV("iso-d", expired, "ImportInProgress", []string{"True", "Pod is running"}),
			deleted: false,
		},
		{
			name:    "expired, non-terminal, no error condition → kept",
			dv:      cacheDV("iso-e", expired, "ImportScheduled", []string{"False", "Pending"}),
			deleted: false,
		},
		{
			name:    "fresh stalled import → kept until TTL",
			dv:      cacheDV("iso-f", fresh, "ImportInProgress", []string{"False", "Error"}),
			deleted: false,
		},
		{
			name:    "fresh succeeded import → kept",
			dv:      cacheDV("iso-g", fresh, PhaseSucceeded, nil),
			deleted: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(tc.dv).Build()
			iso := &ISOImporter{Client: cl, OperatorNS: "op-ns"}

			if err := iso.CleanupExpiredISOs(context.Background(), "op-ns", 24*time.Hour); err != nil {
				t.Fatalf("CleanupExpiredISOs: %v", err)
			}

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(dvGVK)
			err := cl.Get(context.Background(), types.NamespacedName{Name: tc.dv.GetName(), Namespace: "op-ns"}, got)
			gone := err != nil
			if gone != tc.deleted {
				t.Errorf("DV deleted = %v, want %v (get err: %v)", gone, tc.deleted, err)
			}
		})
	}
}

func TestCleanupExpiredISOsSkipsClones(t *testing.T) {
	dv := cacheDV("iso-clone", time.Now().Add(-48*time.Hour), PhaseSucceeded, nil)
	labels := dv.GetLabels()
	labels["ruddervirt.io/iso-clone"] = "true"
	dv.SetLabels(labels)

	cl := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(dv).Build()
	iso := &ISOImporter{Client: cl, OperatorNS: "op-ns"}

	if err := iso.CleanupExpiredISOs(context.Background(), "op-ns", 24*time.Hour); err != nil {
		t.Fatalf("CleanupExpiredISOs: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(dvGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "iso-clone", Namespace: "op-ns"}, got); err != nil {
		t.Errorf("clone DV should have been kept, got err: %v", err)
	}
}
