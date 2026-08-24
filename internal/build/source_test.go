// SPDX-License-Identifier: GPL-3.0-only

package build

import (
	"context"
	"testing"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseNamespacedName(t *testing.T) {
	tests := []struct {
		input   string
		wantNS  string
		wantN   string
		wantErr bool
	}{
		{"images/my-dv", "images", "my-dv", false},
		{"default/output-vol", "default", "output-vol", false},
		{"no-slash", "", "", true},
		{"a/b/c", "a", "b/c", false}, // first slash wins
	}

	for _, tt := range tests {
		ns, name, err := parseNamespacedName(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseNamespacedName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if ns != tt.wantNS || name != tt.wantN {
			t.Errorf("parseNamespacedName(%q) = (%q, %q), want (%q, %q)", tt.input, ns, name, tt.wantNS, tt.wantN)
		}
	}
}

// sourceCacheDV builds a source-cache DataVolume in the given import
// state, mirroring iso_test.go's cacheDV for SourceImporter.ensureCacheDV's
// twin cache-import code path. transitioned sets the Running condition's
// lastTransitionTime — cacheImportError's grace period keys off that, not
// the DataVolume's creation time.
func sourceCacheDV(name string, transitioned time.Time, phase string, runningCond []string) *unstructured.Unstructured {
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(dvGVK)
	dv.SetName(name)
	dv.SetNamespace("op-ns")
	status := map[string]any{"phase": phase}
	if runningCond != nil {
		status["conditions"] = []any{
			map[string]any{
				"type": "Running", "status": runningCond[0], "reason": runningCond[1],
				"message": "connection refused", "lastTransitionTime": transitioned.UTC().Format(time.RFC3339),
			},
		}
	}
	dv.Object["status"] = status
	return dv
}

// TestSourceImporter_EnsureCacheDV_FailsFastOnStalledImport is the
// SourceImporter twin of iso_test.go's ISO regression test: a cached
// source DataVolume (URL/containerDisk import) whose importer pod is
// crashlooping without CDI ever setting status.phase to Failed must fail
// fast past the grace period instead of polling forever.
func TestSourceImporter_EnsureCacheDV_FailsFastOnStalledImport(t *testing.T) {
	old := time.Now().Add(-10 * time.Minute)
	dv := sourceCacheDV("src-deadurl", old, "ImportInProgress", []string{"False", "Error"})

	cl := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(dv).Build()
	s := &SourceImporter{Client: cl, OperatorNS: "op-ns"}

	_, err := s.ensureCacheDV(context.Background(), &v1alpha1.BuildVM{}, &resolvedSource{}, "src-deadurl", "op-ns")
	if err == nil {
		t.Fatal("ensureCacheDV: want error for a stalled import past the grace period, got nil")
	}
}

// TestSourceImporter_EnsureCacheDV_StillWaitingWithinGracePeriod guards
// the grace period: a freshly-created cache DV that's already stalled
// must not fail immediately.
func TestSourceImporter_EnsureCacheDV_StillWaitingWithinGracePeriod(t *testing.T) {
	fresh := time.Now()
	dv := sourceCacheDV("src-fresh", fresh, "ImportInProgress", []string{"False", "Error"})

	cl := fake.NewClientBuilder().WithScheme(isoScheme(t)).WithObjects(dv).Build()
	s := &SourceImporter{Client: cl, OperatorNS: "op-ns"}

	ready, err := s.ensureCacheDV(context.Background(), &v1alpha1.BuildVM{}, &resolvedSource{}, "src-fresh", "op-ns")
	if err != nil {
		t.Fatalf("ensureCacheDV: want nil error within the grace period, got %v", err)
	}
	if ready {
		t.Error("ensureCacheDV: ready = true, want false (still importing)")
	}
}
