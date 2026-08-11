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

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func capturingHandlerFloppyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("adding batchv1 to scheme: %v", err)
	}
	return s
}

// capturingHandlerFloppyBuild returns a build whose only VM already finished
// disk capture (OutputDataVolume set) and uses a floppy — the minimal
// fixture needed to isolate the floppy-cleanup gate from disk-capture logic.
func capturingHandlerFloppyBuild() *v1alpha1.VirtualMachineBuild {
	return &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: "default"},
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{{
				Name:   "installer",
				Floppy: &v1alpha1.Floppy{Files: []v1alpha1.FloppyFileRef{{Name: "Autounattend.xml"}}},
			}},
		},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:        "bld123",
			BuildNamespace: "ns",
			VMStatuses: []v1alpha1.VMBuildStatus{{
				Name:             "installer",
				OutputDataVolume: "ns/bld-out-installer",
			}},
		},
	}
}

// TestCapturingHandler_WaitsForFloppyCleanup confirms the phase does not
// advance to TemplateProvisioning while a VM's floppy.img is still on its
// (shared) efivars PVC — that PVC is reused verbatim by the persisted
// template and restored verbatim into every clone, and floppy injection is
// gated purely by file presence (cmd/sidecar/main.go), so leaving the file in
// place would leak a phantom floppy device into every template and clone.
func TestCapturingHandler_WaitsForFloppyCleanup(t *testing.T) {
	vmBuild := capturingHandlerFloppyBuild()
	c := fake.NewClientBuilder().WithScheme(capturingHandlerFloppyScheme(t)).WithObjects(vmBuild).Build()

	h := &capturingHandler{client: c}
	phase, err := h.Handle(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if phase != v1alpha1.BuildPhaseCapturingDisks {
		t.Errorf("phase = %v, want %v (must not advance before floppy cleanup completes)", phase, v1alpha1.BuildPhaseCapturingDisks)
	}

	// EnsureFloppyCleanup should have created the cleanup Job as a side effect.
	jobName := build.FloppyCleanupJobName(vmBuild.Status.BuildID, "installer")
	job := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: jobName, Namespace: "ns"}, job); err != nil {
		t.Errorf("expected cleanup Job %s to be created: %v", jobName, err)
	}
}

// TestCapturingHandler_AdvancesAfterFloppyCleanup confirms the phase advances
// to TemplateProvisioning once the floppy-cleanup Job has completed.
func TestCapturingHandler_AdvancesAfterFloppyCleanup(t *testing.T) {
	vmBuild := capturingHandlerFloppyBuild()
	efiPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bld123-installer-efivars",
			Namespace: "ns",
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      build.FloppyCleanupJobName(vmBuild.Status.BuildID, "installer"),
			Namespace: "ns",
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(capturingHandlerFloppyScheme(t)).
		WithObjects(vmBuild, efiPVC, job).
		Build()

	h := &capturingHandler{client: c}
	phase, err := h.Handle(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if phase != v1alpha1.BuildPhaseTemplateProvisioning {
		t.Errorf("phase = %v, want %v", phase, v1alpha1.BuildPhaseTemplateProvisioning)
	}
}
