package network

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureSubnet creates a KubeOVN Subnet and its NAD if they don't exist.
// The namespace parameter controls where the NAD is created.
func EnsureSubnet(ctx context.Context, c client.Client, subnetName, vpcName, namespace string, spec SubnetSpec, internetEnabled bool, labels map[string]string) error {
	nadName := subnetName + "-nad"
	provider := fmt.Sprintf("%s.%s.ovn", nadName, namespace)

	// Create the subnet if it doesn't exist.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "Subnet",
	})
	err := c.Get(ctx, types.NamespacedName{Name: subnetName}, existing)
	if err == nil && existing.GetDeletionTimestamp() != nil {
		return fmt.Errorf("subnet %s is being deleted, waiting for cleanup", subnetName)
	}
	if errors.IsNotFound(err) {
		labelsIface := make(map[string]any, len(labels))
		for k, v := range labels {
			labelsIface[k] = v
		}

		subnetSpec := map[string]any{
			"vpc":         vpcName,
			"cidrBlock":   spec.CIDR,
			"protocol":    "IPv4",
			"enableDHCP":  spec.DHCP,
			"natOutgoing": internetEnabled,
			"provider":    provider,
			"gatewayType": "distributed",
		}

		if len(spec.ExcludeIPs) > 0 {
			excl := make([]any, len(spec.ExcludeIPs))
			for i, e := range spec.ExcludeIPs {
				excl[i] = e
			}
			subnetSpec["excludeIps"] = excl
		}
		if spec.DisableGatewayCheck {
			subnetSpec["disableGatewayCheck"] = true
		}
		if spec.Gateway != "" {
			subnetSpec["gateway"] = spec.Gateway
		}

		// Configure DNS via DHCP options so VMs receive nameservers.
		// Always emit something when DHCP is on: virt-launcher's bridge-mode
		// DHCP fallback otherwise leaks the pod's resolv.conf (Kubernetes
		// service DNS + cluster search domains) into the guest.
		if spec.DHCP {
			if dns := SubnetDNS(spec.CIDR, spec.DNS, internetEnabled); dns != "" {
				// KubeOVN parses dhcpV4Options as a comma-separated list of
				// key=value pairs, so a comma inside dns_server={...} (e.g. a
				// multi-resolver "8.8.8.8,1.1.1.1") splits the value and leaves
				// dns_server malformed ("{8.8.8.8"). OVN's put_dhcp_opts then
				// fails to build the reply and the guest gets NO DHCP offer at
				// all. KubeOVN accepts ';' as the in-set separator and rewrites
				// it to ',' when programming OVN, so encode multi-server lists
				// with semicolons.
				ovnDNS := strings.ReplaceAll(dns, ",", ";")
				subnetSpec["dhcpV4Options"] = fmt.Sprintf("dns_server={%s}", ovnDNS)
			}
		}

		subnet := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "kubeovn.io/v1",
				"kind":       "Subnet",
				"metadata": map[string]any{
					"name":   subnetName,
					"labels": labelsIface,
				},
				"spec": subnetSpec,
			},
		}

		if err := c.Create(ctx, subnet); err != nil {
			return fmt.Errorf("creating subnet: %w", err)
		}
	} else if err != nil {
		return err
	}

	return EnsureNAD(ctx, c, nadName, namespace, provider, labels)
}

// EnsureNAD creates a NetworkAttachmentDefinition if it doesn't exist.
func EnsureNAD(ctx context.Context, c client.Client, nadName, namespace, provider string, labels map[string]string) error {
	existingNAD := &unstructured.Unstructured{}
	existingNAD.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition",
	})
	err := c.Get(ctx, types.NamespacedName{Name: nadName, Namespace: namespace}, existingNAD)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	labelsIface := make(map[string]any, len(labels))
	for k, v := range labels {
		labelsIface[k] = v
	}

	nad := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]any{
				"name":      nadName,
				"namespace": namespace,
				"labels":    labelsIface,
			},
			"spec": map[string]any{
				"config": fmt.Sprintf(`{
  "cniVersion": "0.3.1",
  "type": "kube-ovn",
  "server_socket": "/run/openvswitch/kube-ovn-daemon.sock",
  "provider": "%s"
}`, provider),
			},
		},
	}

	return c.Create(ctx, nad)
}

// DeleteSubnet deletes a KubeOVN Subnet by name.
func DeleteSubnet(ctx context.Context, c client.Client, name string) error {
	subnet := &unstructured.Unstructured{}
	subnet.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "Subnet",
	})
	subnet.SetName(name)

	if err := c.Delete(ctx, subnet); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting Subnet %s: %w", name, err)
	}
	return nil
}

// CleanupOrphanedIPs deletes KubeOVN IP objects that belong to a build.
// These can become orphaned when subnets are deleted before IPs are released.
// buildID is the unique build identifier (e.g. "vm-abc123") used to match IP names.
func CleanupOrphanedIPs(ctx context.Context, c client.Client, buildID string) error {
	ipList := &unstructured.UnstructuredList{}
	ipList.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "IP",
	})
	// KubeOVN IPs don't have our labels, but their names contain the buildID.
	// List all and filter by buildID prefix.
	if err := c.List(ctx, ipList); err != nil {
		return fmt.Errorf("listing IPs: %w", err)
	}
	prefix := buildID
	for i := range ipList.Items {
		ip := &ipList.Items[i]
		name := ip.GetName()
		if len(name) < len(prefix) {
			continue
		}
		// IP names follow the pattern: {podName}.{namespace}.{provider}
		// Pod names start with "aileron-{buildName}-"
		if !containsSubstring(name, prefix) {
			continue
		}
		// Remove finalizers first, then delete.
		if len(ip.GetFinalizers()) > 0 {
			ip.SetFinalizers(nil)
			if err := c.Update(ctx, ip); err != nil && !errors.IsNotFound(err) {
				continue
			}
		}
		_ = c.Delete(ctx, ip)
	}
	return nil
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s[:len(substr)] == substr || strings.Contains(s, substr))
}

// SubnetGateway returns the first usable IPv4 address in a CIDR, which
// matches KubeOVN's default gateway placement (network address + 1).
func SubnetGateway(cidr string) string {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	gw := ip.Mask(ipNet.Mask).To4()
	if gw == nil {
		return ""
	}
	gw[3]++
	return gw.String()
}

// UnmanagedGateway returns the second-to-last usable IPv4 address in a CIDR
// (e.g. 192.168.1.253 for 192.168.1.0/24) — the address ApplyUnmanaged parks
// KubeOVN's mandatory gateway router port on so it can't shadow the guest
// gateway at the first usable IP. Errors when the CIDR is narrower than /29,
// the smallest prefix where the two stay distinct with headroom for hosts.
func UnmanagedGateway(cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parsing CIDR %q: %w", cidr, err)
	}
	ip4 := ipNet.IP.To4()
	if ip4 == nil {
		return "", fmt.Errorf("CIDR %q is not IPv4", cidr)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 {
		return "", fmt.Errorf("CIDR %q is not IPv4", cidr)
	}
	if ones > 29 {
		return "", fmt.Errorf("unmanaged subnet CIDR %q is too small: need /29 or wider so the OVN gateway (second-to-last usable IP) stays clear of the guest gateway (first usable IP)", cidr)
	}
	base := binary.BigEndian.Uint32(ip4)
	size := uint32(1) << (32 - uint32(ones))
	gw := make(net.IP, 4)
	binary.BigEndian.PutUint32(gw, base+size-3) // broadcast-2 = second-to-last usable
	return gw.String(), nil
}

// SubnetDNS returns the comma-separated DNS server list for a subnet, applying
// the priority: explicit DNS → public DNS when internet is reachable → the
// subnet's gateway IP as a non-routable placeholder for air-gapped subnets.
// Used both for the OVN dhcpV4Options string and (via SubnetDNSServers) for
// the VMI dnsConfig.nameservers so the two paths can never disagree.
func SubnetDNS(cidr, dns string, internetEnabled bool) string {
	if dns != "" {
		return dns
	}
	if internetEnabled {
		return "8.8.8.8,1.1.1.1"
	}
	return SubnetGateway(cidr)
}

// SubnetDNSServers returns the same value as SubnetDNS, split into a slice
// suitable for a VMI's spec.template.spec.dnsConfig.nameservers field.
func SubnetDNSServers(cidr, dns string, internetEnabled bool) []string {
	raw := SubnetDNS(cidr, dns, internetEnabled)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// IsSubnetReady checks if a KubeOVN Subnet has been validated.
func IsSubnetReady(ctx context.Context, c client.Client, name string) (bool, error) {
	subnet := &unstructured.Unstructured{}
	subnet.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubeovn.io", Version: "v1", Kind: "Subnet",
	})
	err := c.Get(ctx, types.NamespacedName{Name: name}, subnet)
	if err != nil {
		return false, err
	}

	conditions, _, _ := unstructured.NestedSlice(subnet.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		condStatus, _ := cond["status"].(string)
		if (condType == "Validated" || condType == "Ready") && condStatus == "True" {
			return true, nil
		}
	}

	return false, nil
}
