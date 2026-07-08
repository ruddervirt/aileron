package clone

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSelectSnapshotClass(t *testing.T) {
	tests := []struct {
		name          string
		candidates    []snapClassInfo
		wantName      string
		wantAmbiguous bool
	}{
		{
			name:       "empty",
			candidates: nil,
			wantName:   "",
		},
		{
			name:       "single match uses it regardless of marks",
			candidates: []snapClassInfo{{name: "only"}},
			wantName:   "only",
		},
		{
			name: "aileron mark beats default and unmarked",
			candidates: []snapClassInfo{
				{name: "def", isDefault: true},
				{name: "picked", aileronMarked: true},
				{name: "plain"},
			},
			wantName: "picked",
		},
		{
			name: "default used when no aileron mark",
			candidates: []snapClassInfo{
				{name: "plain"},
				{name: "def", isDefault: true},
			},
			wantName: "def",
		},
		{
			name: "multiple aileron-marked -> sorted first, ambiguous",
			candidates: []snapClassInfo{
				{name: "zeta", aileronMarked: true},
				{name: "alpha", aileronMarked: true},
			},
			wantName:      "alpha",
			wantAmbiguous: true,
		},
		{
			name: "multiple unmarked -> sorted first, ambiguous",
			candidates: []snapClassInfo{
				{name: "beta"},
				{name: "alpha"},
			},
			wantName:      "alpha",
			wantAmbiguous: true,
		},
		{
			name: "aileron mark wins even if another class is default and sorts first",
			candidates: []snapClassInfo{
				{name: "aaa-default", isDefault: true},
				{name: "zzz-aileron", aileronMarked: true},
			},
			wantName: "zzz-aileron",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotAmbiguous := selectSnapshotClass(tt.candidates)
			if gotName != tt.wantName {
				t.Errorf("name = %q, want %q", gotName, tt.wantName)
			}
			if gotAmbiguous != tt.wantAmbiguous {
				t.Errorf("ambiguous = %v, want %v", gotAmbiguous, tt.wantAmbiguous)
			}
		})
	}
}

const testDriver = "rook-ceph.rbd.csi.ceph.com"

// snapClass builds a VolumeSnapshotClass unstructured for the fake client.
func snapClass(name, driver string, anns map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass",
	})
	u.SetName(name)
	if len(anns) > 0 {
		u.SetAnnotations(anns)
	}
	_ = unstructured.SetNestedField(u.Object, driver, "driver")
	return u
}

// snapshotClassScheme registers the unstructured VolumeSnapshotClass list kind so
// the fake client can serve resolveSnapshotClass's List call.
func snapshotClassScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass",
	}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClassList",
	}, &unstructured.UnstructuredList{})
	return scheme
}

func TestResolveSnapshotClass_AileronMarkedWins(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(snapshotClassScheme()).
		WithObjects(
			snapClass("backup-class", testDriver, nil),
			snapClass("primary-class", testDriver, map[string]string{AnnotationSnapshotClass: "true"}),
		).
		Build()
	sm := &SnapshotManager{Client: c}

	name, warning, err := sm.resolveSnapshotClass(context.Background(), testDriver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "primary-class" {
		t.Errorf("name = %q, want primary-class", name)
	}
	if warning != "" {
		t.Errorf("warning = %q, want empty", warning)
	}
}

func TestResolveSnapshotClass_AmbiguousWarns(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(snapshotClassScheme()).
		WithObjects(
			snapClass("zeta-class", testDriver, nil),
			snapClass("alpha-class", testDriver, nil),
		).
		Build()
	sm := &SnapshotManager{Client: c}

	name, warning, err := sm.resolveSnapshotClass(context.Background(), testDriver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "alpha-class" {
		t.Errorf("name = %q, want alpha-class (deterministic sorted pick)", name)
	}
	if warning == "" {
		t.Fatal("want non-empty ambiguity warning, got empty")
	}
	if !strings.Contains(warning, AnnotationSnapshotClass) {
		t.Errorf("warning should name the disambiguation annotation, got %q", warning)
	}
}

func TestResolveSnapshotClass_NoMatchErrors(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(snapshotClassScheme()).
		WithObjects(snapClass("other", "some.other.driver", nil)).
		Build()
	sm := &SnapshotManager{Client: c}

	if _, _, err := sm.resolveSnapshotClass(context.Background(), testDriver); err == nil {
		t.Fatal("want error for driver with no matching VolumeSnapshotClass, got nil")
	}
}

func TestResolveSnapshotClass_CachesResult(t *testing.T) {
	only := snapClass("only-class", testDriver, nil)
	c := fake.NewClientBuilder().
		WithScheme(snapshotClassScheme()).
		WithObjects(only).
		Build()
	sm := &SnapshotManager{Client: c}
	ctx := context.Background()

	name, _, err := sm.resolveSnapshotClass(ctx, testDriver)
	if err != nil || name != "only-class" {
		t.Fatalf("first resolve: name=%q err=%v", name, err)
	}

	// Delete the class from the cluster; a cached resolve must still succeed.
	if err := c.Delete(ctx, only); err != nil {
		t.Fatalf("deleting class: %v", err)
	}
	name, _, err = sm.resolveSnapshotClass(ctx, testDriver)
	if err != nil {
		t.Fatalf("cached resolve errored: %v", err)
	}
	if name != "only-class" {
		t.Errorf("cached name = %q, want only-class", name)
	}
}
