package build

import (
	"context"
	"fmt"
	"strings"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	relayImage = "busybox:latest"
	// HTTPPort is the port the httpd sidecar listens on in the relay pod.
	HTTPPort = 8080
)

// RelayPodManager manages the lifecycle of relay pods used to tunnel SSH
// connections to VMs on VPC subnets, and optionally serves HTTP content.
type RelayPodManager struct {
	Client client.Client
}

// RelayPodName returns the relay pod name for a build.
func RelayPodName(buildID string) string {
	return fmt.Sprintf("%s-relay", buildID)
}

// EnsureRelayPod creates a relay pod attached to all build subnets if it doesn't exist.
func (r *RelayPodManager) EnsureRelayPod(ctx context.Context, build *v1alpha1.VirtualMachineBuild) error {
	logger := log.FromContext(ctx)
	buildID := BuildID(build)
	buildNS := BuildNS(build)
	podName := RelayPodName(buildID)

	existing := &corev1.Pod{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: podName, Namespace: buildNS}, existing)
	if err == nil {
		// If relay exited (Succeeded/Failed), delete it so it gets recreated.
		if existing.Status.Phase == corev1.PodSucceeded || existing.Status.Phase == corev1.PodFailed {
			logger.Info("Relay pod exited, deleting for recreation", "pod", podName, "phase", existing.Status.Phase)
			_ = r.Client.Delete(ctx, existing)
			return fmt.Errorf("relay pod exited (%s), waiting for recreation", existing.Status.Phase)
		}
		return nil // Already exists and running.
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("checking relay pod: %w", err)
	}

	logger.Info("Creating relay pod", "pod", podName)
	pod := r.buildRelayPod(build)
	if err := r.Client.Create(ctx, pod); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating relay pod: %w", err)
	}

	// Clean up any stale relay pods for this build (e.g., from old naming conventions).
	staleList := &corev1.PodList{}
	if listErr := r.Client.List(ctx, staleList,
		client.InNamespace(buildNS),
		client.MatchingLabels{
			LabelBuild:     build.Name,
			LabelComponent: "relay",
		},
	); listErr == nil {
		for i := range staleList.Items {
			if staleList.Items[i].Name != podName {
				logger.Info("Cleaning up stale relay pod", "pod", staleList.Items[i].Name)
				_ = r.Client.Delete(ctx, &staleList.Items[i])
			}
		}
	}

	return nil
}

// IsRelayReady checks if the relay pod is Running.
// Also cleans up any stale relay pods with wrong names.
func (r *RelayPodManager) IsRelayReady(ctx context.Context, build *v1alpha1.VirtualMachineBuild) (bool, error) {
	logger := log.FromContext(ctx)
	buildID := BuildID(build)
	buildNS := BuildNS(build)
	podName := RelayPodName(buildID)

	pod := &corev1.Pod{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: podName, Namespace: buildNS}, pod); err != nil {
		return false, fmt.Errorf("getting relay pod: %w", err)
	}

	// Clean up stale relay pods with wrong names (e.g., from old naming conventions).
	staleList := &corev1.PodList{}
	if listErr := r.Client.List(ctx, staleList,
		client.InNamespace(buildNS),
		client.MatchingLabels{
			LabelBuild:     build.Name,
			LabelComponent: "relay",
		},
	); listErr == nil {
		for i := range staleList.Items {
			if staleList.Items[i].Name != podName {
				logger.Info("Cleaning up stale relay pod", "pod", staleList.Items[i].Name)
				_ = r.Client.Delete(ctx, &staleList.Items[i])
			}
		}
	}

	switch pod.Status.Phase {
	case corev1.PodRunning:
		return true, nil
	case corev1.PodFailed, corev1.PodSucceeded:
		return false, fmt.Errorf("relay pod %s is in terminal phase %s", podName, pod.Status.Phase)
	default:
		return false, nil // still pending/creating, requeue
	}
}

// CleanupRelayPod deletes the relay pod.
func (r *RelayPodManager) CleanupRelayPod(ctx context.Context, build *v1alpha1.VirtualMachineBuild) error {
	buildID := BuildID(build)
	buildNS := BuildNS(build)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: RelayPodName(buildID), Namespace: buildNS},
	}
	if err := r.Client.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting relay pod: %w", err)
	}
	return nil
}

func (r *RelayPodManager) buildRelayPod(build *v1alpha1.VirtualMachineBuild) *corev1.Pod {
	buildID := BuildID(build)
	buildNS := BuildNS(build)
	podAnnotations := r.buildRelayAnnotations(build, buildID, buildNS)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RelayPodName(buildID),
			Namespace: buildNS,
			Labels: map[string]string{
				LabelBuildID:        buildID,
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
				LabelComponent:      "relay",
			},
			Annotations: podAnnotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "relay",
					Image:   relayImage,
					Command: []string{"sleep", "infinity"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("50m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("200m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
					},
				},
			},
		},
	}

	// Add httpd sidecar when httpDirectory is configured.
	if build.Spec.HttpDirectory != nil {
		httpdContainer := corev1.Container{
			Name:    "httpd",
			Image:   relayImage,
			Command: []string{"busybox", "httpd", "-f", "-p", fmt.Sprintf("%d", HTTPPort), "-h", "/http"},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "httpdir", MountPath: "/http", ReadOnly: true},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("16Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				},
			},
		}

		if len(build.Spec.HttpDirectory.Files) > 0 {
			// emptyDir populated by init container from files ConfigMap + URL downloads.
			httpdContainer.VolumeMounts[0].ReadOnly = false // init writes here

			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name:         "httpdir",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})

			// Mount the files ConfigMap so the init container can copy inline files.
			cmName := FilesConfigMapName(buildID)
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: "files-cm",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
					},
				},
			})

			// Build init script: copy inline files (with path mapping) and download URL files.
			script := r.buildHTTPInitScript(build)
			pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
				Name:    "http-files",
				Image:   relayImage,
				Command: []string{"sh", "-c", script},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "httpdir", MountPath: "/http"},
					{Name: "files-cm", MountPath: "/files-cm", ReadOnly: true},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			})
		}

		pod.Spec.Containers = append(pod.Spec.Containers, httpdContainer)
	}

	return pod
}

// buildHTTPInitScript generates a shell script that populates /http from
// inline files (copied from ConfigMap) and URL files (downloaded with wget).
func (r *RelayPodManager) buildHTTPInitScript(build *v1alpha1.VirtualMachineBuild) string {
	var cmds []string
	for _, ref := range build.Spec.HttpDirectory.Files {
		f, err := ResolveFile(build, ref.Name)
		if err != nil {
			continue // Validation should catch this before we get here.
		}

		if f.Inline != "" {
			cmds = append(cmds, fmt.Sprintf("cp /files-cm/%s /http/%s", f.Name, ref.Name))
		} else if f.URL != "" {
			cmds = append(cmds, fmt.Sprintf("wget -q -O /http/%s '%s'", ref.Name, f.URL))
		}
	}
	return strings.Join(cmds, " && ")
}

// GetRelayHTTPIP resolves the relay pod's IP address on the VM's first NIC subnet.
// Derives the annotation key from the relay pod's own network annotation.
func GetRelayHTTPIP(ctx context.Context, k8sClient client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) (string, error) {
	buildID := BuildID(build)
	buildNS := BuildNS(build)

	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: RelayPodName(buildID), Namespace: buildNS}, pod); err != nil {
		return "", fmt.Errorf("getting relay pod: %w", err)
	}

	// Derive the NAD name from the relay pod's own network annotation. The
	// HTTP IP must live on a managed (OVN) subnet because the relay only
	// attaches to those; pick the VM's first managed NIC.
	networksAnnotation := pod.Annotations["k8s.v1.cni.cncf.io/networks"]
	if networksAnnotation == "" {
		return "", fmt.Errorf("relay pod missing k8s.v1.cni.cncf.io/networks annotation")
	}

	mgmtNIC, ok := FirstManagedNIC(build, effectiveVMNICs(build, vmSpec))
	if !ok {
		return "", fmt.Errorf("VM %q has no managed NIC; relay HTTP unreachable", vmSpec.Name)
	}
	suffix := "-" + mgmtNIC.Subnet + "-subnet-nad"
	var nadName string
	for ref := range strings.SplitSeq(networksAnnotation, ",") {
		ref = strings.TrimSpace(ref)
		parts := strings.SplitN(ref, "/", 2)
		if len(parts) == 2 && strings.HasSuffix(parts[1], suffix) {
			nadName = parts[1]
			break
		}
	}
	if nadName == "" {
		return "", fmt.Errorf("relay pod networks annotation has no NAD matching subnet %q: %s", mgmtNIC.Subnet, networksAnnotation)
	}

	provider := fmt.Sprintf("%s.%s.ovn", nadName, buildNS)
	annotationKey := provider + ".kubernetes.io/ip_address"

	ip, ok := pod.Annotations[annotationKey]
	if !ok || ip == "" {
		return "", fmt.Errorf("relay pod missing KubeOVN IP annotation %s", annotationKey)
	}
	return ip, nil
}

// buildRelayAnnotations generates the Multus and KubeOVN annotations for the
// relay pod. buildID is used for resource name prefixes (NAD names), buildNS
// is the Kubernetes namespace where NADs live.
//
// Unmanaged subnets are skipped: the relay reaches VMs over OVN-IPAM'd ports,
// and unmanaged subnets exclude their entire usable CIDR from OVN's IPAM so
// the relay's LSP can't get an IP there. VMs that use unmanaged subnets must
// keep at least one managed NIC for the relay to reach them. The effective
// (buildOverride-aware) flag is used so a segment overridden managed for the
// build IS attached to the relay.
func (r *RelayPodManager) buildRelayAnnotations(build *v1alpha1.VirtualMachineBuild, buildID, buildNS string) map[string]string {
	if build.Spec.Network == nil {
		return nil
	}

	annotations := make(map[string]string)
	var nadRefs []string

	for _, subnet := range build.Spec.Network.Subnets {
		if effectiveSubnetUnmanaged(build, subnet.Name) {
			continue
		}
		subnetName := buildSubnetName(buildID, subnet.Name)
		nadName := subnetName + "-nad"
		provider := fmt.Sprintf("%s.%s.ovn", nadName, buildNS)

		nadRefs = append(nadRefs, fmt.Sprintf("%s/%s", buildNS, nadName))

		// KubeOVN annotations for subnet assignment.
		annotations[provider+".kubernetes.io/logical_switch"] = subnetName
	}

	if len(nadRefs) > 0 {
		annotations["k8s.v1.cni.cncf.io/networks"] = strings.Join(nadRefs, ",")
	}

	return annotations
}
