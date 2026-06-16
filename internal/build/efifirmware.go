package build

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	efiPVCSuffix     = "-efivars"
	efiCopyJobSuffix = "-eficopy"
	efiPVCSize       = "256Mi"

	// The eficopy and floppy populator Jobs run once and then expire via
	// TTLSecondsAfterFinished. Once the Job is GC'd the only signal that
	// "data is on the PVC" disappears, so subsequent reconciles recreate
	// the Job for the lifetime of the build. To keep that signal durable
	// we stamp the EFI PVC with these annotations after observing each
	// Job's success — the populator runs exactly once per build.
	annotationEFIPopulated    = "aileron.ruddervirt.io/efi-populated"
	annotationFloppyPopulated = "aileron.ruddervirt.io/floppy-populated"

	valueTrue = "true"
)

// efiPVCHasAnnotation returns true when the EFI PVC for this VM carries the
// given annotation set to "true". Missing PVC is reported as not-populated
// so the caller can fall through to creating it.
func efiPVCHasAnnotation(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, key string) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      efiPVCName(BuildID(build), vmSpec.Name),
		Namespace: BuildNS(build),
	}, pvc)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return pvc.Annotations[key] == valueTrue, nil
}

// markEFIPVCAnnotation stamps a "true" annotation onto the EFI PVC. Idempotent.
func markEFIPVCAnnotation(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, key string) error {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      efiPVCName(BuildID(build), vmSpec.Name),
		Namespace: BuildNS(build),
	}, pvc); err != nil {
		return err
	}
	if pvc.Annotations[key] == valueTrue {
		return nil
	}
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[key] = valueTrue
	return c.Update(ctx, pvc)
}

// efiPVCName returns the PVC name for EFI firmware files.
func efiPVCName(buildID, vmName string) string {
	return fmt.Sprintf("%s-%s%s", buildID, vmName, efiPVCSuffix)
}

// efiCopyJobName returns the Job name for copying firmware files.
func efiCopyJobName(buildID, vmName string) string {
	return fmt.Sprintf("%s-%s%s", buildID, vmName, efiCopyJobSuffix)
}

// operatorImage returns the operator image from the OPERATOR_IMAGE env var.
func operatorImage() string {
	if img := os.Getenv("OPERATOR_IMAGE"); img != "" {
		return img
	}
	return "ghcr.io/ruddervirt/aileron:latest"
}

// sidecarImage returns the sidecar hook image derived from the operator image.
// ghcr.io/ruddervirt/aileron:tag -> ghcr.io/ruddervirt/aileron/sidecar:tag
func sidecarImage() string {
	img := operatorImage()
	if idx := strings.LastIndex(img, ":"); idx > 0 {
		return img[:idx] + "/sidecar" + img[idx:]
	}
	return img + "/sidecar"
}

// buildNamespace returns the Kubernetes namespace for build resources.
// Delegates to BuildNS for the single-namespace model.
func buildNamespace(build *v1alpha1.VirtualMachineBuild) string {
	return BuildNS(build)
}

// EnsureEFIFirmware creates the PVC, copy Job, and hook ConfigMap needed
// for custom EFI firmware on a VM.
//
// When the VM's source is a buildRef, the EFI vars are cloned from the
// parent build's EFI PVC (which contains the boot entries written during
// the parent's OS installation). Otherwise, fresh OVMF files are copied
// from the operator image.
func EnsureEFIFirmware(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) error {
	if err := ensureEFIPVC(ctx, c, build, vmSpec); err != nil {
		return fmt.Errorf("ensuring EFI PVC: %w", err)
	}

	// Once a previous reconcile observed the eficopy job's success, the PVC
	// is annotated as populated. Skip job creation entirely so the job
	// doesn't get recreated after TTLSecondsAfterFinished GC.
	populated, err := efiPVCHasAnnotation(ctx, c, build, vmSpec, annotationEFIPopulated)
	if err != nil {
		return fmt.Errorf("checking EFI PVC populated annotation: %w", err)
	}
	if populated {
		return nil
	}

	// For buildRef sources, clone the parent's EFI vars instead of copying
	// fresh defaults — the parent's vars contain the OS boot entries.
	if vmSpec.Source.BuildRef != nil {
		return ensureEFICopyFromParent(ctx, c, build, vmSpec)
	}

	if err := ensureEFICopyJob(ctx, c, build, vmSpec); err != nil {
		return fmt.Errorf("ensuring EFI copy job: %w", err)
	}
	return nil
}

// ensureEFICopyFromParent creates a Job that copies EFI vars from the parent
// build's EFI PVC into this build's EFI PVC.
func ensureEFICopyFromParent(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) error {
	ref := vmSpec.Source.BuildRef
	refNS := ref.Namespace
	if refNS == "" {
		refNS = build.Namespace
	}

	// Look up the parent build to get its buildID.
	parent := &v1alpha1.VirtualMachineBuild{}
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: refNS}, parent); err != nil {
		return fmt.Errorf("looking up parent build %s: %w", ref.Name, err)
	}

	// Find the parent VM name.
	parentVMName := ref.VMName
	if parentVMName == "" && len(parent.Status.VMStatuses) == 1 {
		parentVMName = parent.Status.VMStatuses[0].Name
	}
	if parentVMName == "" {
		return fmt.Errorf("cannot determine parent VM name for EFI vars clone")
	}

	parentEFIPVC := efiPVCName(BuildID(parent), parentVMName)

	// Verify the parent EFI PVC exists.
	parentPVC := &corev1.PersistentVolumeClaim{}
	parentNS := BuildNS(parent)
	if err := c.Get(ctx, types.NamespacedName{Name: parentEFIPVC, Namespace: parentNS}, parentPVC); err != nil {
		if errors.IsNotFound(err) {
			// Parent has no EFI PVC — fall back to default copy.
			return ensureEFICopyJob(ctx, c, build, vmSpec)
		}
		return fmt.Errorf("checking parent EFI PVC %s: %w", parentEFIPVC, err)
	}

	// Create a Job that mounts both PVCs and copies the parent's files.
	jobName := efiCopyJobName(BuildID(build), vmSpec.Name)
	ns := buildNamespace(build)

	existing := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, existing); err == nil {
		return nil // Already exists.
	} else if !errors.IsNotFound(err) {
		return err
	}

	var pullSecrets []corev1.LocalObjectReference
	if names := os.Getenv("IMAGE_PULL_SECRETS"); names != "" {
		for name := range strings.SplitSeq(names, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
			}
		}
	}

	targetPVC := efiPVCName(BuildID(build), vmSpec.Name)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
			Labels: map[string]string{
				LabelBuildID:        BuildID(build),
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
				LabelVM:             vmSpec.Name,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ttl(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelBuildID:   BuildID(build),
						LabelComponent: "efi-copy",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					ImagePullSecrets: pullSecrets,
					Containers: []corev1.Container{
						{
							Name:    "copy-efi-vars",
							Image:   "busybox:stable",
							Command: []string{"sh", "-c", "cp -a /src/* /dst/"},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "src", MountPath: "/src", ReadOnly: true},
								{Name: "dst", MountPath: "/dst"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "src",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: parentEFIPVC,
									ReadOnly:  true,
								},
							},
						},
						{
							Name: "dst",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: targetPVC,
								},
							},
						},
					},
				},
			},
		},
	}

	if DebugMode() {
		job.Spec.TTLSecondsAfterFinished = nil
	}

	if err := c.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating EFI copy-from-parent job: %w", err)
	}
	return nil
}

// IsEFIFirmwareReady checks whether the EFI firmware copy Job has completed.
// On first observation of a successful Job, stamps the EFI PVC with an
// annotation so future reconciles can short-circuit even after the Job is
// garbage-collected by TTLSecondsAfterFinished.
func IsEFIFirmwareReady(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) (bool, error) {
	populated, err := efiPVCHasAnnotation(ctx, c, build, vmSpec, annotationEFIPopulated)
	if err != nil {
		return false, fmt.Errorf("checking EFI PVC populated annotation: %w", err)
	}
	if populated {
		return true, nil
	}

	jobName := efiCopyJobName(BuildID(build), vmSpec.Name)
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: jobName, Namespace: buildNamespace(build)}, job); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			if err := markEFIPVCAnnotation(ctx, c, build, vmSpec, annotationEFIPopulated); err != nil {
				return true, fmt.Errorf("marking EFI PVC populated: %w", err)
			}
			return true, nil
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return false, fmt.Errorf("EFI firmware copy job failed: %s", cond.Message)
		}
	}
	return false, nil
}

// BuildHookSidecarsAnnotation returns the JSON value for the
// hooks.kubevirt.io/hookSidecars annotation. Uses the aileron/sidecar image
// which contains a compiled Go binary for domain XML modification.
// Returns empty string if no hooks are needed.
func BuildHookSidecarsAnnotation(buildID string, vmSpec *v1alpha1.BuildVM) (string, error) {
	if vmSpec.EFIFirmware == nil && vmSpec.Floppy == nil {
		return "", nil
	}

	hook := map[string]any{
		"args":  []string{"--version", "v1alpha2"},
		"image": sidecarImage(),
	}

	// The EFI PVC holds firmware files AND the floppy ISO image.
	// Mount it into the sidecar whenever either feature is used.
	if vmSpec.EFIFirmware != nil || vmSpec.Floppy != nil {
		hook["pvc"] = map[string]any{
			"name":              efiPVCName(buildID, vmSpec.Name),
			"volumePath":        "/efivars",
			"sharedComputePath": "/var/run/efivars",
		}
	}

	hooks := []map[string]any{hook}
	data, err := json.Marshal(hooks)
	if err != nil {
		return "", fmt.Errorf("marshaling hook sidecar annotation: %w", err)
	}
	return string(data), nil
}

func ensureEFIPVC(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) error {
	name := efiPVCName(BuildID(build), vmSpec.Name)

	existing := &corev1.PersistentVolumeClaim{}
	ns := buildNamespace(build)
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing); err == nil {
		return nil // Already exists.
	} else if !errors.IsNotFound(err) {
		return err
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelBuildID:        BuildID(build),
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
				LabelVM:             vmSpec.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(efiPVCSize),
				},
			},
		},
	}

	if err := c.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func ensureEFICopyJob(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) error {
	jobName := efiCopyJobName(BuildID(build), vmSpec.Name)
	pvcName := efiPVCName(BuildID(build), vmSpec.Name)

	existing := &batchv1.Job{}
	ns := buildNamespace(build)
	if err := c.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, existing); err == nil {
		// If the job is being deleted, wait for it to finish.
		if existing.DeletionTimestamp != nil {
			return fmt.Errorf("waiting for stale EFI copy job %s to be deleted", jobName)
		}
		// Validate the image matches our current version. If not, delete and recreate.
		if len(existing.Spec.Template.Spec.Containers) > 0 &&
			existing.Spec.Template.Spec.Containers[0].Image != operatorImage() {
			propagation := metav1.DeletePropagationBackground
			if err := c.Delete(ctx, existing, &client.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("replacing stale EFI copy job: %w", err)
			}
			// Return and let next reconcile create with correct image.
			return fmt.Errorf("deleted stale EFI copy job %s, waiting for cleanup", jobName)
		}
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	// Build imagePullSecrets from env var (comma-separated secret names).
	var pullSecrets []corev1.LocalObjectReference
	if names := os.Getenv("IMAGE_PULL_SECRETS"); names != "" {
		for name := range strings.SplitSeq(names, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
			}
		}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
			Labels: map[string]string{
				LabelBuildID:        BuildID(build),
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
				LabelVM:             vmSpec.Name,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ttl(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelBuildID:   BuildID(build),
						LabelComponent: "efi-copy",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					ImagePullSecrets: pullSecrets,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true), //nolint:modernize
						RunAsUser:    ptr.To[int64](65532),
						// FSGroup must match qemu's gid in virt-launcher (107).
						// Kubelet chowns the PVC mount to this group with the
						// setgid bit so files copied here inherit gid=107, and
						// later when virt-launcher mounts the same PVC, qemu
						// (uid=107) can create the writable OVMF_VARS_LIVE.fd
						// alongside the firmware files. Using 65532 here causes
						// libvirt SyncVMI to fail with "Permission denied" on
						// OVMF_VARS_LIVE.fd.new.
						FSGroup: ptr.To[int64](107),
					},
					Containers: []corev1.Container{
						{
							Name:    "copy-firmware",
							Image:   operatorImage(),
							Command: []string{"/manager", "copy-firmware", "/efi"},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "efi",
									MountPath: "/efi",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "efi",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := c.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// DebugMode reports whether the operator is running in debug mode.
// Debug mode is enabled by setting DEBUG=true on the controller deployment.
func DebugMode() bool {
	return os.Getenv("DEBUG") == valueTrue
}

// ttl returns a pointer to seconds for use as Job.Spec.TTLSecondsAfterFinished,
// or nil when debug mode is enabled so that finished Jobs and their pods stick
// around for inspection instead of being cleaned up.
func ttl() *int32 {
	if DebugMode() {
		return nil
	}
	v := int32(300)
	return &v
}

// FailureRetention returns how long failed VirtualMachineBuild resources are
// retained before the controller cleans them up. Controlled by the
// FAILURE_RETENTION envvar (Go duration string). Returns 0 if unset, empty,
// or unparseable — meaning cleanup happens immediately.
func FailureRetention() time.Duration {
	v := os.Getenv("FAILURE_RETENTION")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}
