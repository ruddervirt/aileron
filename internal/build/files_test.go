// SPDX-License-Identifier: GPL-3.0-only

package build

import (
	"context"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func floppyCleanupScheme(t *testing.T) *runtime.Scheme {
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

func floppyCleanupFixtures() (*v1alpha1.VirtualMachineBuild, *v1alpha1.BuildVM, *corev1.PersistentVolumeClaim) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: "default"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld123", BuildNamespace: "ns"},
	}
	vmSpec := &v1alpha1.BuildVM{
		Name:   "installer",
		Floppy: &v1alpha1.Floppy{Files: []v1alpha1.FloppyFileRef{{Name: "Autounattend.xml"}}},
	}
	efiPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      efiPVCName(BuildID(build), vmSpec.Name),
			Namespace: BuildNS(build),
		},
	}
	return build, vmSpec, efiPVC
}

func TestEnsureFloppyCleanup_NoOpWithoutFloppy(t *testing.T) {
	build, vmSpec, _ := floppyCleanupFixtures()
	vmSpec.Floppy = nil

	c := fake.NewClientBuilder().WithScheme(floppyCleanupScheme(t)).Build()

	if err := EnsureFloppyCleanup(context.Background(), c, build, vmSpec); err != nil {
		t.Fatalf("EnsureFloppyCleanup: %v", err)
	}

	jobs := &batchv1.JobList{}
	if err := c.List(context.Background(), jobs); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("expected no Job created without vmSpec.Floppy, got %d", len(jobs.Items))
	}
}

func TestEnsureFloppyCleanup_CreatesJob(t *testing.T) {
	build, vmSpec, efiPVC := floppyCleanupFixtures()

	c := fake.NewClientBuilder().WithScheme(floppyCleanupScheme(t)).WithObjects(efiPVC).Build()

	if err := EnsureFloppyCleanup(context.Background(), c, build, vmSpec); err != nil {
		t.Fatalf("EnsureFloppyCleanup: %v", err)
	}

	job := &batchv1.Job{}
	jobName := FloppyCleanupJobName(BuildID(build), vmSpec.Name)
	if err := c.Get(context.Background(), types.NamespacedName{Name: jobName, Namespace: BuildNS(build)}, job); err != nil {
		t.Fatalf("expected cleanup Job %s to be created: %v", jobName, err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if len(container.Command) < 3 || container.Command[2] != "rm -f /efi/floppy.img" {
		t.Errorf("cleanup Job command = %v, want rm -f /efi/floppy.img", container.Command)
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].MountPath != "/efi" {
		t.Errorf("cleanup Job volume mounts = %v, want a single /efi mount", container.VolumeMounts)
	}

	// Calling again while the Job already exists must not error or recreate it.
	if err := EnsureFloppyCleanup(context.Background(), c, build, vmSpec); err != nil {
		t.Fatalf("EnsureFloppyCleanup (idempotent call): %v", err)
	}
}

func TestIsFloppyCleanupReady_NoOpWithoutFloppy(t *testing.T) {
	build, vmSpec, _ := floppyCleanupFixtures()
	vmSpec.Floppy = nil

	c := fake.NewClientBuilder().WithScheme(floppyCleanupScheme(t)).Build()

	ready, err := IsFloppyCleanupReady(context.Background(), c, build, vmSpec)
	if err != nil {
		t.Fatalf("IsFloppyCleanupReady: %v", err)
	}
	if !ready {
		t.Error("expected ready=true when vmSpec.Floppy is nil (nothing to clean up)")
	}
}

func TestIsFloppyCleanupReady_NotReadyBeforeJobExists(t *testing.T) {
	build, vmSpec, efiPVC := floppyCleanupFixtures()

	c := fake.NewClientBuilder().WithScheme(floppyCleanupScheme(t)).WithObjects(efiPVC).Build()

	ready, err := IsFloppyCleanupReady(context.Background(), c, build, vmSpec)
	if err != nil {
		t.Fatalf("IsFloppyCleanupReady: %v", err)
	}
	if ready {
		t.Error("expected ready=false before the cleanup Job exists")
	}
}

func TestIsFloppyCleanupReady_JobRunning(t *testing.T) {
	build, vmSpec, efiPVC := floppyCleanupFixtures()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      FloppyCleanupJobName(BuildID(build), vmSpec.Name),
			Namespace: BuildNS(build),
		},
	}

	c := fake.NewClientBuilder().WithScheme(floppyCleanupScheme(t)).WithObjects(efiPVC, job).Build()

	ready, err := IsFloppyCleanupReady(context.Background(), c, build, vmSpec)
	if err != nil {
		t.Fatalf("IsFloppyCleanupReady: %v", err)
	}
	if ready {
		t.Error("expected ready=false while the cleanup Job has no Complete condition")
	}
}

func TestIsFloppyCleanupReady_JobComplete(t *testing.T) {
	build, vmSpec, efiPVC := floppyCleanupFixtures()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      FloppyCleanupJobName(BuildID(build), vmSpec.Name),
			Namespace: BuildNS(build),
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(floppyCleanupScheme(t)).WithObjects(efiPVC, job).Build()

	ready, err := IsFloppyCleanupReady(context.Background(), c, build, vmSpec)
	if err != nil {
		t.Fatalf("IsFloppyCleanupReady: %v", err)
	}
	if !ready {
		t.Fatal("expected ready=true once the cleanup Job is Complete")
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: efiPVC.Name, Namespace: efiPVC.Namespace}, pvc); err != nil {
		t.Fatalf("getting EFI PVC: %v", err)
	}
	if pvc.Annotations[annotationFloppyCleaned] != valueTrue {
		t.Errorf("expected EFI PVC to be stamped %s=true, got annotations=%v", annotationFloppyCleaned, pvc.Annotations)
	}

	// Once stamped, readiness is reported without needing the Job to still exist.
	if err := c.Delete(context.Background(), job); err != nil {
		t.Fatalf("deleting Job: %v", err)
	}
	ready, err = IsFloppyCleanupReady(context.Background(), c, build, vmSpec)
	if err != nil {
		t.Fatalf("IsFloppyCleanupReady after Job GC: %v", err)
	}
	if !ready {
		t.Error("expected ready=true from the PVC annotation alone after the Job is garbage-collected")
	}
}

func TestIsFloppyCleanupReady_JobFailed(t *testing.T) {
	build, vmSpec, efiPVC := floppyCleanupFixtures()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      FloppyCleanupJobName(BuildID(build), vmSpec.Name),
			Namespace: BuildNS(build),
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "boom"},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(floppyCleanupScheme(t)).WithObjects(efiPVC, job).Build()

	ready, err := IsFloppyCleanupReady(context.Background(), c, build, vmSpec)
	if err == nil {
		t.Fatal("expected an error when the cleanup Job failed")
	}
	if ready {
		t.Error("expected ready=false when the cleanup Job failed")
	}
}
