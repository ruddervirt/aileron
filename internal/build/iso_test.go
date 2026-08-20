package build

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
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

// withRunningTransition sets the Running condition's lastTransitionTime
// on dv, for cacheImportError grace-period tests. dv must already carry a
// Running condition (cacheDV with a non-nil runningCond).
func withRunningTransition(dv *unstructured.Unstructured, transitioned time.Time) *unstructured.Unstructured {
	conditions := dv.Object["status"].(map[string]any)["conditions"].([]any)
	for _, item := range conditions {
		cond := item.(map[string]any)
		if cond["type"] == "Running" {
			cond["lastTransitionTime"] = transitioned.UTC().Format(time.RFC3339)
		}
	}
	return dv
}

func TestCacheImportError(t *testing.T) {
	old := time.Now().Add(-1 * time.Hour)
	fresh := time.Now()

	cases := []struct {
		name    string
		dv      *unstructured.Unstructured
		wantErr bool
	}{
		{
			name:    "stalled and past grace period → error",
			dv:      withRunningTransition(cacheDV("iso-a", old, "ImportInProgress", []string{"False", "Error"}), old),
			wantErr: true,
		},
		{
			name:    "stalled but within grace period → nil",
			dv:      withRunningTransition(cacheDV("iso-b", fresh, "ImportInProgress", []string{"False", "Error"}), fresh),
			wantErr: false,
		},
		{
			name:    "actively downloading, old → nil",
			dv:      withRunningTransition(cacheDV("iso-c", old, "ImportInProgress", []string{"True", "Pod is running"}), old),
			wantErr: false,
		},
		{
			name:    "non-terminal, no error condition, old → nil",
			dv:      withRunningTransition(cacheDV("iso-d", old, "ImportScheduled", []string{"False", "Pending"}), old),
			wantErr: false,
		},
		{
			// The exact scenario cacheImportError's anchor is designed for:
			// a long-running, otherwise-healthy import (DV itself is old)
			// that just had its Running condition flip to Error a moment
			// ago — a transient blip, not proof of a permanently dead
			// source. No creation timestamp is set on this DV at all,
			// proving the result depends only on the condition's own
			// transition time, never on the DataVolume's age.
			name:    "Running just transitioned to Error, DV otherwise old → nil (fresh blip, not yet stalled)",
			dv:      withRunningTransition(cacheDV("iso-e", time.Now(), "ImportInProgress", []string{"False", "Error"}), fresh),
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cacheImportError(tc.dv)
			if (err != nil) != tc.wantErr {
				t.Errorf("cacheImportError() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestISOImporter_EnsureCacheDV_FailsFastOnStalledImport reproduces
// base-kali-a5tcqtnf-7vzzr on rudderusa: an ISO cache DataVolume whose
// source URL 404s. CDI never sets status.phase to Failed for this — it
// crashloops the importer pod forever — so before this fix the build
// polled "still importing" until Spec.Timeout (2h). It must now fail fast
// with the real CDI error once the grace period has elapsed.
func TestISOImporter_EnsureCacheDV_FailsFastOnStalledImport(t *testing.T) {
	old := time.Now().Add(-10 * time.Minute)
	dv := withRunningTransition(cacheDV("aileron-iso-deadurl", old, "ImportInProgress",
		[]string{"False", "Error"}), old)
	// Match the real message observed on the cluster.
	dv.Object["status"].(map[string]any)["conditions"].([]any)[1].(map[string]any)["message"] =
		"Unable to connect to http data source: expected status code 200, got 404. Status: 404 Not Found"

	cl := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(dv).Build()
	iso := &ISOImporter{Client: cl, OperatorNS: "op-ns"}

	_, err := iso.ensureCacheDV(context.Background(), "op-ns", "aileron-iso-deadurl",
		&v1alpha1.ISOSource{URL: "https://cdimage.kali.org/kali-2025.3/kali-linux-2025.3-installer-netinst-amd64.iso"},
		"deadurl-cache-key")
	if err == nil {
		t.Fatal("ensureCacheDV: want error for a stalled import past the grace period, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("ensureCacheDV error = %q, want it to include the underlying 404", err.Error())
	}
}

// TestISOImporter_EnsureCacheDV_StillWaitingWithinGracePeriod guards the
// grace period itself: a freshly-created cache DV that's already stalled
// must not fail immediately — CDI gets a chance to retry first.
func TestISOImporter_EnsureCacheDV_StillWaitingWithinGracePeriod(t *testing.T) {
	fresh := time.Now()
	dv := withRunningTransition(cacheDV("aileron-iso-fresh", fresh, "ImportInProgress",
		[]string{"False", "Error"}), fresh)

	cl := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(dv).Build()
	iso := &ISOImporter{Client: cl, OperatorNS: "op-ns"}

	ready, err := iso.ensureCacheDV(context.Background(), "op-ns", "aileron-iso-fresh",
		&v1alpha1.ISOSource{URL: "https://example.test/some.iso"}, "fresh-cache-key")
	if err != nil {
		t.Fatalf("ensureCacheDV: want nil error within the grace period, got %v", err)
	}
	if ready {
		t.Error("ensureCacheDV: ready = true, want false (still importing)")
	}
}
