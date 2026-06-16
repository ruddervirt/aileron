package network

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ovnSubnetsIPSet = "ovn40subnets"
	ovnOVSNamespace = "kube-system"
	ovnOVSLabel     = "app=ovs"
	ovnOVSContainer = "openvswitch"
)

// EnsureIPSetEntry adds a CIDR to the ovn40subnets ipset on all nodes.
// This is needed because KubeOVN only adds default overlay subnets to the ipset,
// but custom VPC subnets (used for build/clone networks) also need to be in the
// ipset so that DHCP responses from OVN can traverse the Linux bridge inside
// KubeVirt virt-launcher pods without being dropped by KUBE-FORWARD iptables rules.
func EnsureIPSetEntry(ctx context.Context, k8sClient client.Client, restConfig *rest.Config, cidr string) error {
	return modifyIPSet(ctx, k8sClient, restConfig, "add", cidr)
}

// RemoveIPSetEntry removes a CIDR from the ovn40subnets ipset on all nodes.
func RemoveIPSetEntry(ctx context.Context, k8sClient client.Client, restConfig *rest.Config, cidr string) error {
	return modifyIPSet(ctx, k8sClient, restConfig, "del", cidr)
}

// RemoveIPSetEntryIfUnused removes a CIDR from the ovn40subnets ipset only
// when no remaining KubeOVN Subnet still declares it. Builds and clones
// routinely share CIDRs (every clone of a lab uses the same 192.168.1.0/24,
// say) and the ipset holds a single entry per CIDR, so removing it while
// another owner still uses that CIDR would silently break their bridge-mode
// DHCP. Call this only AFTER the caller's own subnets are deleted (e.g. after
// TeardownNetwork reports done) — otherwise they count as users and the entry
// is kept.
func RemoveIPSetEntryIfUnused(ctx context.Context, k8sClient client.Client, restConfig *rest.Config, cidr string) error {
	user, err := cidrInUse(ctx, k8sClient, cidr)
	if err != nil {
		return err
	}
	if user != "" {
		// A terminating subnet still counts: its owner's deletion flow
		// re-evaluates once it is gone.
		log.FromContext(ctx).Info("Keeping ovn40subnets ipset entry; CIDR still declared by a subnet",
			"cidr", cidr, "subnet", user)
		return nil
	}
	return RemoveIPSetEntry(ctx, k8sClient, restConfig, cidr)
}

// cidrInUse returns the name of any remaining KubeOVN Subnet that declares the
// CIDR, or "" when none does.
func cidrInUse(ctx context.Context, k8sClient client.Client, cidr string) (string, error) {
	subnets := &unstructured.UnstructuredList{}
	subnets.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "SubnetList",
	})
	if err := k8sClient.List(ctx, subnets); err != nil {
		return "", fmt.Errorf("listing KubeOVN subnets: %w", err)
	}
	for i := range subnets.Items {
		block, _, _ := unstructured.NestedString(subnets.Items[i].Object, "spec", "cidrBlock")
		if block == cidr {
			return subnets.Items[i].GetName(), nil
		}
	}
	return "", nil
}

func modifyIPSet(ctx context.Context, k8sClient client.Client, restConfig *rest.Config, action, cidr string) error {
	logger := log.FromContext(ctx)

	// List all kube-ovn OVS pods.
	var podList corev1.PodList
	if err := k8sClient.List(ctx, &podList,
		client.InNamespace(ovnOVSNamespace),
		client.MatchingLabels{"app": "ovs"},
	); err != nil {
		return fmt.Errorf("listing OVS pods: %w", err)
	}

	if len(podList.Items) == 0 {
		return fmt.Errorf("no OVS pods found in %s", ovnOVSNamespace)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating clientset: %w", err)
	}

	// The -exist flag prevents errors when adding a CIDR that already exists
	// or deleting one that doesn't.
	var flag string
	if action == "add" {
		flag = "-exist"
	} else {
		flag = "-exist"
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		cmd := []string{"ipset", action, flag, ovnSubnetsIPSet, cidr}
		if err := execInPod(ctx, clientset, restConfig, pod.Namespace, pod.Name, ovnOVSContainer, cmd); err != nil {
			logger.Error(err, "Failed to modify ipset on OVS pod", "pod", pod.Name, "action", action, "cidr", cidr)
			// Continue to other pods — best effort.
			continue
		}
		logger.Info("Modified ovn40subnets ipset", "pod", pod.Name, "action", action, "cidr", cidr)
	}

	return nil
}

func execInPod(ctx context.Context, clientset kubernetes.Interface, restConfig *rest.Config, namespace, podName, container string, cmd []string) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec %v: %w (stderr: %s)", cmd, err, stderr.String())
	}

	return nil
}
