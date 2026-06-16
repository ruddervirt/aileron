package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// coordinatorImage returns the coordinator pod image.
func coordinatorImage() string {
	if img := os.Getenv("COORDINATOR_IMAGE"); img != "" {
		return img
	}
	base := operatorImage()
	if idx := strings.LastIndex(base, ":"); idx > 0 {
		repo := base[:idx]
		tag := base[idx:]
		return repo + "-coordinator" + tag
	}
	return base + "-coordinator"
}

// CoordinatorConfigMapName returns the name of the coordinator config ConfigMap.
func CoordinatorConfigMapName(buildID, vmName string) string {
	return fmt.Sprintf("%s-coordinator-%s", buildID, vmName)
}

// CoordinatorJobName returns the name of the coordinator Job.
func CoordinatorJobName(buildID, vmName string) string {
	return fmt.Sprintf("%s-coordinator-%s", buildID, vmName)
}

// CoordinatorJobOpts are the runtime parameters for the coordinator Job.
type CoordinatorJobOpts struct {
	VMIName      string
	VNCURL       string
	RelayPodName string
	NICName      string
	SSHPort      int32
	StatusCMName string
	SSHKeySecret string
}

// EnsureCoordinatorConfigMap creates a ConfigMap with the full coordinator config.
func EnsureCoordinatorConfigMap(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, cfg *CoordinatorConfig) error {
	buildID := BuildID(build)
	ns := buildNamespace(build)
	cmName := CoordinatorConfigMapName(buildID, vmSpec.Name)

	existing := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: cmName, Namespace: ns}, existing); err == nil {
		return nil
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling coordinator config: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: ns,
			Labels: map[string]string{
				LabelBuildID:        buildID,
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
				LabelComponent:      "coordinator",
			},
		},
		Data: map[string]string{
			"config.json": string(data),
		},
	}

	if err := c.Create(ctx, cm); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating coordinator ConfigMap: %w", err)
	}
	return nil
}

// EnsureCoordinatorJob creates the coordinator Job.
func EnsureCoordinatorJob(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, opts CoordinatorJobOpts) error {
	buildID := BuildID(build)
	ns := buildNamespace(build)
	jobName := CoordinatorJobName(buildID, vmSpec.Name)
	cmName := CoordinatorConfigMapName(buildID, vmSpec.Name)

	existing := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, existing); err == nil {
		return nil
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

	env := []corev1.EnvVar{
		{Name: "VNC_URL", Value: opts.VNCURL},
		{Name: "BOOT_NS", Value: ns},
		{Name: "BOOT_VMI", Value: opts.VMIName},
		{Name: "BOOT_BUILD", Value: build.Name},
		{Name: "BOOT_VM_NAME", Value: vmSpec.Name},
		{Name: "COORDINATOR_CONFIG", Value: "/etc/coordinator/config.json"},
	}

	volumes := []corev1.Volume{
		{
			Name: "coordinator-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				},
			},
		},
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "coordinator-config", MountPath: "/etc/coordinator", ReadOnly: true},
	}

	// Add SSH/provisioning env vars and mounts if provisioners are present.
	if len(vmSpec.Provisioners) > 0 {
		env = append(env,
			corev1.EnvVar{Name: "RELAY_POD_NAME", Value: opts.RelayPodName},
			corev1.EnvVar{Name: "VM_NIC_NAME", Value: opts.NICName},
			corev1.EnvVar{Name: "STATUS_CONFIGMAP", Value: opts.StatusCMName},
			corev1.EnvVar{Name: "SSH_KEY_PATH", Value: "/etc/coordinator/ssh/id"},
		)

		// Mount SSH key Secret if it exists.
		if opts.SSHKeySecret != "" {
			optional := true
			volumes = append(volumes, corev1.Volume{
				Name: "ssh-key",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: opts.SSHKeySecret,
						Items: []corev1.KeyToPath{
							{Key: "id_ed25519", Path: "id"},
						},
						Optional: &optional,
					},
				},
			})
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name: "ssh-key", MountPath: "/etc/coordinator/ssh", ReadOnly: true,
			})
		}
	}

	// Use the controller's ServiceAccount for pods/exec and configmap access.
	saName := os.Getenv("COORDINATOR_SERVICE_ACCOUNT")
	if saName == "" {
		saName = "aileron-controller-manager"
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
			Labels: map[string]string{
				LabelBuildID:        buildID,
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
				LabelVM:             vmSpec.Name,
				LabelComponent:      "coordinator",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ttl(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelBuildID:   buildID,
						LabelComponent: "coordinator",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: saName,
					RestartPolicy:      corev1.RestartPolicyNever,
					ImagePullSecrets:   pullSecrets,
					Containers: []corev1.Container{
						{
							Name:  "coordinator",
							Image: coordinatorImage(),
							// Always: the manager deploys with pullPolicy Always
							// too; IfNotPresent left stale same-tag coordinator
							// images running on nodes after a re-push.
							ImagePullPolicy: corev1.PullAlways,
							Env:             env,
							VolumeMounts:    volumeMounts,
							// A small request floor makes the Job Burstable
							// rather than BestEffort, so it isn't first in line
							// for the OOM killer when a node is packed (e.g.
							// many concurrent builds during a busy exam). It
							// idles waiting on VNC/SSH, so the floor is tiny.
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	if err := c.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating coordinator Job: %w", err)
	}
	return nil
}

// CoordinatorJobStatus checks the status of the coordinator Job. When the
// Job has failed and restConfig is non-nil, the last log line from the
// failed coordinator pod is appended to the returned message — the Job's
// own "Job has reached the specified backoff limit" condition message is
// not actionable on its own.
func CoordinatorJobStatus(ctx context.Context, c client.Client, restConfig *rest.Config, build *v1alpha1.VirtualMachineBuild, vmName string) (string, string, error) {
	buildID := BuildID(build)
	ns := buildNamespace(build)
	jobName := CoordinatorJobName(buildID, vmName)

	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
		if errors.IsNotFound(err) {
			return PhaseRunning, "Job not found yet", nil
		}
		return "", "", err
	}

	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return PhaseSucceeded, "", nil
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			msg := cond.Message
			if detail := failedCoordinatorPodTail(ctx, c, restConfig, ns, jobName); detail != "" {
				msg = msg + ": " + detail
			}
			return PhaseFailed, msg, nil
		}
	}

	return PhaseRunning, "", nil
}

// failedCoordinatorPodTail returns the last non-empty log line from the
// failed pod owned by jobName, or "" if it can't be retrieved. Best-effort:
// any error reading logs is swallowed since the caller already has a
// fallback message from the Job's Failed condition.
func failedCoordinatorPodTail(ctx context.Context, c client.Client, restConfig *rest.Config, ns, jobName string) string {
	if restConfig == nil {
		return ""
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return ""
	}

	pods := &corev1.PodList{}
	if err := c.List(ctx, pods,
		client.InNamespace(ns),
		client.MatchingLabels{"batch.kubernetes.io/job-name": jobName},
	); err != nil || len(pods.Items) == 0 {
		return ""
	}

	// Prefer the pod that actually failed; otherwise fall back to the
	// most recently created one.
	var pod *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodFailed {
			pod = p
			break
		}
	}
	if pod == nil {
		pod = &pods.Items[len(pods.Items)-1]
	}

	tail := int64(20)
	req := clientset.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: "coordinator",
		TailLines: &tail,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}

	for _, line := range reverseLines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 500 {
			line = line[:500] + "...(truncated)"
		}
		return line
	}
	return ""
}

// reverseLines splits s on newlines and returns the lines in reverse order.
func reverseLines(s string) []string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[len(parts)-1-i] = p
	}
	return out
}

// AllCoordinatorsComplete returns true when every VM that has provisioners
// has a completed (succeeded) coordinator Job. VMs with no boot commands
// and no provisioners are skipped.
func AllCoordinatorsComplete(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild) bool {
	for i := range build.Spec.VMs {
		vm := &build.Spec.VMs[i]
		if len(vm.BootCommand) == 0 && len(vm.Provisioners) == 0 {
			continue
		}
		status, _, err := CoordinatorJobStatus(ctx, c, nil, build, vm.Name)
		if err != nil || status != PhaseSucceeded {
			return false
		}
	}
	return true
}
