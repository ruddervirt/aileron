package build

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PodDiskReader reads a PVC's contents by spawning a temporary pod that mounts
// the PVC and streams data via `kubectl exec cat`.
type PodDiskReader struct {
	Client     client.Client
	RESTConfig *rest.Config
}

const (
	diskReaderImage = "busybox:latest"
	diskMountPath   = "/disk"
	diskFileName    = "disk.img"
)

func (r *PodDiskReader) ReadDisk(ctx context.Context, pvcName, namespace string) (io.ReadCloser, error) {
	logger := log.FromContext(ctx)
	podName := fmt.Sprintf("%s-reader", pvcName)

	// Check if reader pod already exists.
	existing := &corev1.Pod{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("checking reader pod: %w", err)
	}

	if errors.IsNotFound(err) {
		logger.Info("Creating disk reader pod", "pod", podName, "pvc", pvcName)
		pod := r.buildReaderPod(podName, namespace, pvcName)
		if err := r.Client.Create(ctx, pod); err != nil {
			return nil, fmt.Errorf("creating reader pod: %w", err)
		}
	}

	// Wait for pod to be running.
	if err := r.waitForPodRunning(ctx, podName, namespace); err != nil {
		return nil, fmt.Errorf("waiting for reader pod: %w", err)
	}

	// Exec into the pod and cat the disk image.
	clientset, err := kubernetes.NewForConfig(r.RESTConfig)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		Param("container", "reader").
		Param("command", "cat").
		Param("command", fmt.Sprintf("%s/%s", diskMountPath, diskFileName)).
		Param("stdout", "true")

	exec, err := remotecommand.NewSPDYExecutor(r.RESTConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("creating executor: %w", err)
	}

	// Use a pipe to stream the output.
	pr, pw := io.Pipe()

	go func() {
		streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: pw,
		})
		pw.CloseWithError(streamErr)

		// Cleanup: delete the reader pod after streaming.
		_ = r.Client.Delete(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		})
	}()

	return pr, nil
}

func (r *PodDiskReader) buildReaderPod(name, namespace, pvcName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelComponent: "disk-reader",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "reader",
					Image:   diskReaderImage,
					Command: []string{"sleep", "3600"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "disk",
							MountPath: diskMountPath,
							ReadOnly:  true,
						},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "disk",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
							ReadOnly:  true,
						},
					},
				},
			},
		},
	}
}

func (r *PodDiskReader) waitForPodRunning(ctx context.Context, name, namespace string) error {
	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("reader pod %s/%s did not become running within timeout", namespace, name)
		case <-ticker.C:
			pod := &corev1.Pod{}
			if err := r.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod); err != nil {
				continue
			}
			if pod.Status.Phase == corev1.PodRunning {
				return nil
			}
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				return fmt.Errorf("reader pod %s/%s is in phase %s", namespace, name, pod.Status.Phase)
			}
		}
	}
}
