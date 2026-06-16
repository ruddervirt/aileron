package build

import (
	"context"
	"fmt"
	"os"
	"strings"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// FilesConfigMapName returns the ConfigMap name for materialized build files.
func FilesConfigMapName(buildID string) string {
	return fmt.Sprintf("%s-files", buildID)
}

// FloppyConfigMapName returns the ConfigMap name for a VM's floppy files.
func FloppyConfigMapName(buildID, vmName string) string {
	return fmt.Sprintf("%s-floppy-%s", buildID, vmName)
}

// FloppyPVCName returns the PVC name for a VM's floppy disk image.
func FloppyPVCName(buildID, vmName string) string {
	return fmt.Sprintf("%s-floppy-img-%s", buildID, vmName)
}

// FloppyJobName returns the Job name for building a floppy disk image.
func FloppyJobName(buildID, vmName string) string {
	return fmt.Sprintf("%s-floppy-job-%s", buildID, vmName)
}

// EnsureFilesConfigMap creates a ConfigMap from inline spec.files entries.
// URL-sourced files are not included — they are downloaded by init containers
// at the point of use (relay pod, etc.).
func EnsureFilesConfigMap(ctx context.Context, k8sClient client.Client, build *v1alpha1.VirtualMachineBuild) error {
	if len(build.Spec.Files) == 0 {
		return nil
	}

	// Collect inline files.
	data := make(map[string]string)
	for _, f := range build.Spec.Files {
		if f.Inline != "" {
			data[f.Name] = f.Inline
		}
	}
	if len(data) == 0 {
		return nil
	}

	logger := log.FromContext(ctx)
	buildID := BuildID(build)
	buildNS := BuildNS(build)
	cmName := FilesConfigMapName(buildID)

	existing := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: cmName, Namespace: buildNS}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("checking files ConfigMap: %w", err)
	}

	logger.Info("Creating files ConfigMap", "name", cmName, "keys", len(data))
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: buildNS,
			Labels: map[string]string{
				LabelBuildID:        buildID,
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
				LabelComponent:      "files",
			},
		},
		Data: data,
	}

	if err := k8sClient.Create(ctx, cm); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating files ConfigMap: %w", err)
	}
	return nil
}

// EnsureFloppyConfigMap creates a ConfigMap for a VM's floppy disk files.
func EnsureFloppyConfigMap(ctx context.Context, k8sClient client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) error {
	if vmSpec.Floppy == nil || len(vmSpec.Floppy.Files) == 0 {
		return nil
	}

	logger := log.FromContext(ctx)
	buildID := BuildID(build)
	buildNS := BuildNS(build)
	cmName := FloppyConfigMapName(buildID, vmSpec.Name)

	existing := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: cmName, Namespace: buildNS}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("checking floppy ConfigMap: %w", err)
	}

	data := make(map[string]string)
	binaryData := make(map[string][]byte)
	for _, ref := range vmSpec.Floppy.Files {
		f, err := ResolveFile(build, ref.Name)
		if err != nil {
			return fmt.Errorf("floppy file %q: %w", ref.Name, err)
		}
		switch {
		case f.Inline != "":
			data[ref.Name] = f.Inline
		case f.URL != "":
			logger.Info("Downloading floppy file", "name", ref.Name, "url", f.URL)
			body, err := DownloadURL(ctx, f.URL)
			if err != nil {
				return fmt.Errorf("floppy file %q: downloading %s: %w", ref.Name, f.URL, err)
			}
			binaryData[ref.Name] = body
		default:
			return fmt.Errorf("floppy file %q: must have inline or url content", ref.Name)
		}
	}

	logger.Info("Creating floppy ConfigMap", "name", cmName, "vm", vmSpec.Name, "keys", len(data)+len(binaryData))
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: buildNS,
			Labels: map[string]string{
				LabelBuildID:        buildID,
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
				LabelComponent:      "floppy",
				LabelVM:             vmSpec.Name,
			},
		},
		Data:       data,
		BinaryData: binaryData,
	}

	if err := k8sClient.Create(ctx, cm); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating floppy ConfigMap: %w", err)
	}
	return nil
}

// EnsureFloppyImage creates a Job to build a 1.44MB FAT12 floppy disk image
// from the floppy ConfigMap. The image is written to the EFI vars PVC (which
// is already mounted in the sidecar) as /efi/floppy.img. The sidecar Go binary
// then injects a real floppy device (isa-fdc, drive 'fda') pointing at this
// file via the shared compute path. A real floppy gets drive letter A: in
// Windows Setup, which is searched for autounattend.xml before any CDROM —
// this beats the install ISO's bundled autounattend to the punch.
//
// Build VMs run on the pc-i440fx machine type (set in vm.go) because RHEL's
// qemu-kvm has a downstream patch that gates isa-fdc off for q35.
func EnsureFloppyImage(ctx context.Context, k8sClient client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) error {
	if vmSpec.Floppy == nil || len(vmSpec.Floppy.Files) == 0 {
		return nil
	}

	logger := log.FromContext(ctx)
	buildID := BuildID(build)
	buildNS := BuildNS(build)
	jobName := FloppyJobName(buildID, vmSpec.Name)
	cmName := FloppyConfigMapName(buildID, vmSpec.Name)
	// Write floppy image to the EFI PVC.
	efiPVC := efiPVCName(buildID, vmSpec.Name)

	// The floppy image is written to the EFI PVC — ensure it exists even
	// when EFIFirmware is not configured on this VM.
	if err := ensureEFIPVC(ctx, k8sClient, build, vmSpec); err != nil {
		return fmt.Errorf("ensuring EFI PVC for floppy: %w", err)
	}

	// If a previous reconcile observed the floppy Job's success, the PVC is
	// stamped as populated. Skip Job creation so it doesn't get recreated
	// after TTLSecondsAfterFinished GC.
	populated, err := efiPVCHasAnnotation(ctx, k8sClient, build, vmSpec, annotationFloppyPopulated)
	if err != nil {
		return fmt.Errorf("checking floppy PVC populated annotation: %w", err)
	}
	if populated {
		return nil
	}

	// Ensure Job exists.
	existingJob := &batchv1.Job{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: buildNS}, existingJob)
	if errors.IsNotFound(err) {
		// Build a 1.44MB FAT12 floppy image. mtools preserves long filenames
		// (VFAT) by default so 'Autounattend.xml' keeps its case. Drivers
		// referenced by autounattend's <DriverPaths>%configsetroot%\</> are
		// at the floppy root alongside autounattend.xml.
		script := `set -e
mkdir -p /tmp/staging
for f in /floppy-files/*; do
  [ -f "$f" ] && cp "$f" /tmp/staging/
done
echo "=== Staging contents ==="
ls -la /tmp/staging/
TOTAL=$(du -sb /tmp/staging | awk '{print $1}')
# 1.44MB FAT12 has ~1.4MB usable after FAT/root-dir overhead.
LIMIT=1400000
if [ "$TOTAL" -gt "$LIMIT" ]; then
  echo "ERROR: floppy contents are $TOTAL bytes, exceeds $LIMIT byte cap for 1.44MB FAT12" >&2
  exit 1
fi
dd if=/dev/zero of=/efi/floppy.img bs=512 count=2880
mformat -i /efi/floppy.img -f 1440 -v AILERON ::
mcopy -i /efi/floppy.img /tmp/staging/* ::
echo "=== Floppy contents ==="
mdir -i /efi/floppy.img ::
ls -la /efi/floppy.img
echo "Floppy image created successfully"
`
		operatorImage := os.Getenv("OPERATOR_IMAGE")
		// Use the helper image (Alpine with mtools/dosfstools).
		// Derive from OPERATOR_IMAGE by replacing the image name suffix.
		helperImage := "alpine:3.21"
		if operatorImage != "" {
			if idx := strings.LastIndex(operatorImage, ":"); idx > 0 {
				base := operatorImage[:idx]
				tag := operatorImage[idx:]
				// ghcr.io/ruddervirt/aileron:tag -> ghcr.io/ruddervirt/aileron/helper:tag
				helperImage = base + "/helper" + tag
			}
		}

		logger.Info("Creating floppy image Job", "name", jobName, "image", helperImage)
		backoffLimit := int32(2)
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: buildNS,
				Labels: map[string]string{
					LabelBuildID:        buildID,
					LabelBuild:          build.Name,
					LabelBuildNamespace: build.Namespace,
					LabelComponent:      "floppy-image",
					LabelVM:             vmSpec.Name,
				},
			},
			Spec: batchv1.JobSpec{
				BackoffLimit:            &backoffLimit,
				TTLSecondsAfterFinished: ttl(),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							LabelBuildID:   buildID,
							LabelComponent: "floppy-image",
						},
					},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyOnFailure,
						Containers: []corev1.Container{{
							Name:    "mkfloppy",
							Image:   helperImage,
							Command: []string{"/bin/sh", "-c", script},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "floppy-files", MountPath: "/floppy-files", ReadOnly: true},
								{Name: "efi", MountPath: "/efi"},
							},
						}},
						Volumes: []corev1.Volume{
							{
								Name: "floppy-files",
								VolumeSource: corev1.VolumeSource{
									ConfigMap: &corev1.ConfigMapVolumeSource{
										LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
									},
								},
							},
							{
								Name: "efi",
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: efiPVC,
									},
								},
							},
						},
					},
				},
			},
		}

		// Add imagePullSecrets if configured.
		if secrets := os.Getenv("IMAGE_PULL_SECRETS"); secrets != "" {
			for s := range strings.SplitSeq(secrets, ",") {
				s = strings.TrimSpace(s)
				if s != "" {
					job.Spec.Template.Spec.ImagePullSecrets = append(
						job.Spec.Template.Spec.ImagePullSecrets,
						corev1.LocalObjectReference{Name: s},
					)
				}
			}
		}

		if err := k8sClient.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating floppy image Job: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("checking floppy image Job: %w", err)
	}

	return nil
}

// IsFloppyImageReady checks whether the floppy image Job has completed.
// On first observation of a successful Job, stamps the EFI PVC with an
// annotation so future reconciles can short-circuit even after the Job is
// garbage-collected by TTLSecondsAfterFinished.
func IsFloppyImageReady(ctx context.Context, k8sClient client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) (bool, error) {
	if vmSpec.Floppy == nil || len(vmSpec.Floppy.Files) == 0 {
		return true, nil
	}

	populated, err := efiPVCHasAnnotation(ctx, k8sClient, build, vmSpec, annotationFloppyPopulated)
	if err != nil {
		return false, fmt.Errorf("checking floppy PVC populated annotation: %w", err)
	}
	if populated {
		return true, nil
	}

	jobName := FloppyJobName(BuildID(build), vmSpec.Name)
	buildNS := BuildNS(build)

	job := &batchv1.Job{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: buildNS}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking floppy Job: %w", err)
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			if err := markEFIPVCAnnotation(ctx, k8sClient, build, vmSpec, annotationFloppyPopulated); err != nil {
				return true, fmt.Errorf("marking floppy PVC populated: %w", err)
			}
			return true, nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, fmt.Errorf("floppy image Job failed: %s", c.Message)
		}
	}
	return false, nil
}

// ResolveFile finds a BuildFile by name in the build spec.
func ResolveFile(build *v1alpha1.VirtualMachineBuild, name string) (*v1alpha1.BuildFile, error) {
	for i := range build.Spec.Files {
		if build.Spec.Files[i].Name == name {
			return &build.Spec.Files[i], nil
		}
	}
	return nil, fmt.Errorf("file %q not found in spec.files", name)
}
