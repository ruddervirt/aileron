// SPDX-License-Identifier: GPL-3.0-only

package build

import (
	"context"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func resolveBuildRefScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	return s
}

// TestResolveBuildRef_ParentDataDisks confirms resolveBuildRef surfaces a
// buildRef parent's captured secondary disks (not just its boot disk), so
// HandleVM's extra-disk loop can clone them instead of creating them blank.
func TestResolveBuildRef_ParentDataDisks(t *testing.T) {
	parent := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Status: v1alpha1.VirtualMachineBuildStatus{
			Phase: v1alpha1.BuildPhaseSucceeded,
			VMStatuses: []v1alpha1.VMBuildStatus{{
				Name:             "base",
				OutputDataVolume: "tmpl-ns/bld-out-base",
				OutputDataVolumes: []v1alpha1.DiskOutputVolume{
					{Name: "supplemental", DataVolume: "tmpl-ns/bld-out-base-supplemental"},
				},
			}},
		},
	}
	bootPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "bld-out-base", Namespace: "tmpl-ns"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("25Gi")},
			},
		},
	}
	dataPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "bld-out-base-supplemental", Namespace: "tmpl-ns"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(resolveBuildRefScheme(t)).
		WithObjects(parent, bootPVC, dataPVC).
		Build()

	s := &SourceImporter{Client: fakeClient}
	child := &v1alpha1.VirtualMachineBuild{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "default"}}
	vmSpec := &v1alpha1.BuildVM{Name: "server"}
	ref := &v1alpha1.BuildReference{Name: "parent", Namespace: "default"}

	resolved, err := s.resolveBuildRef(context.Background(), child, vmSpec, ref)
	if err != nil {
		t.Fatalf("resolveBuildRef() error = %v", err)
	}

	if resolved.pvcName != "bld-out-base" || resolved.pvcNamespace != "tmpl-ns" {
		t.Errorf("boot disk source = %s/%s, want tmpl-ns/bld-out-base", resolved.pvcNamespace, resolved.pvcName)
	}
	if resolved.sourceSize == nil || resolved.sourceSize.String() != "25Gi" {
		t.Errorf("boot disk sourceSize = %v, want 25Gi", resolved.sourceSize)
	}

	pd, ok := resolved.parentDataDisks["supplemental"]
	if !ok {
		t.Fatalf("parentDataDisks missing %q; got %v", "supplemental", resolved.parentDataDisks)
	}
	if pd.pvcName != "bld-out-base-supplemental" || pd.pvcNamespace != "tmpl-ns" {
		t.Errorf("supplemental source = %s/%s, want tmpl-ns/bld-out-base-supplemental", pd.pvcNamespace, pd.pvcName)
	}
	if pd.sourceSize == nil || pd.sourceSize.String() != "5Gi" {
		t.Errorf("supplemental sourceSize = %v, want 5Gi", pd.sourceSize)
	}

	if _, ok := resolved.parentDataDisks["nonexistent"]; ok {
		t.Error("parentDataDisks should not contain disks the parent never captured")
	}
}

// TestResolveBuildRef_NoSecondaryDisks confirms a parent with only a boot
// disk (no OutputDataVolumes) yields a nil parentDataDisks map, so the
// extra-disk loop falls back to creating any child-declared disk blank.
func TestResolveBuildRef_NoSecondaryDisks(t *testing.T) {
	parent := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-boot-only", Namespace: "default"},
		Status: v1alpha1.VirtualMachineBuildStatus{
			Phase: v1alpha1.BuildPhaseSucceeded,
			VMStatuses: []v1alpha1.VMBuildStatus{{
				Name:             "base",
				OutputDataVolume: "tmpl-ns/bld-out-base",
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(resolveBuildRefScheme(t)).
		WithObjects(parent).
		Build()

	s := &SourceImporter{Client: fakeClient}
	child := &v1alpha1.VirtualMachineBuild{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "default"}}
	vmSpec := &v1alpha1.BuildVM{Name: "server"}
	ref := &v1alpha1.BuildReference{Name: "parent-boot-only", Namespace: "default"}

	resolved, err := s.resolveBuildRef(context.Background(), child, vmSpec, ref)
	if err != nil {
		t.Fatalf("resolveBuildRef() error = %v", err)
	}
	if resolved.parentDataDisks != nil {
		t.Errorf("parentDataDisks = %v, want nil", resolved.parentDataDisks)
	}
}
