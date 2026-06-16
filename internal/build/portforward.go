package build

import (
	"context"
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// GetVMIPFromVMI looks up a VMI's IP address for a specific interface name.
func GetVMIPFromVMI(ctx context.Context, k8sClient client.Client, vmiName, namespace, nicName string) (string, error) {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance",
	})
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: vmiName, Namespace: namespace}, vmi); err != nil {
		return "", fmt.Errorf("getting VMI %s: %w", vmiName, err)
	}

	interfaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	for _, iface := range interfaces {
		ifaceMap, ok := iface.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(ifaceMap, "name")
		if name == nicName {
			ip, _, _ := unstructured.NestedString(ifaceMap, "ipAddress")
			if ip != "" {
				return ip, nil
			}
		}
	}

	return "", fmt.Errorf("VMI %s has no IP for interface %s", vmiName, nicName)
}

// NewRelayDialFunc returns a Dial function that tunnels TCP through a relay pod
// by exec'ing "nc <targetIP> <port>" on it. This allows SSH access to VMs that
// only have bridge NICs (no masquerade/pod network).
func NewRelayDialFunc(restConfig *rest.Config, namespace, relayPodName, targetIP string, port int32) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		logger := log.FromContext(ctx)
		logger.Info("Dialing via relay pod", "relay", relayPodName, "target", targetIP, "port", port)
		return NewExecConn(ctx, restConfig, namespace, relayPodName, targetIP, port)
	}
}
