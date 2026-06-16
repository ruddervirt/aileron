package build

import (
	"context"
	"fmt"
	"slices"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/network"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// NetworkSetup handles the Networking phase: creating KubeOVN VPCs and Subnets
// for inter-VM communication during the build.
type NetworkSetup struct {
	Client     client.Client
	RESTConfig *rest.Config
}

//nolint:gocyclo
func (n *NetworkSetup) Handle(ctx context.Context, build *v1alpha1.VirtualMachineBuild) (v1alpha1.BuildPhase, error) {
	logger := log.FromContext(ctx)

	if build.Spec.Network == nil || (len(build.Spec.Network.VPCs) == 0 && len(build.Spec.Network.Subnets) == 0) {
		logger.Info("No network topology defined, skipping to building")
		return v1alpha1.BuildPhaseBuilding, nil
	}

	buildID := BuildID(build)
	buildNS := BuildNS(build)

	// Initialize network status.
	if build.Status.Network == nil {
		build.Status.Network = &v1alpha1.NetworkStatus{}
	}

	labels := map[string]string{
		LabelBuildID:        buildID,
		LabelBuild:          build.Name,
		LabelBuildNamespace: build.Namespace,
	}

	// Build effective VPC list. If no VPCs are declared but subnets exist,
	// auto-create a default "vpc" with internet matching the build-level setting.
	vpcs := build.Spec.Network.VPCs
	if len(vpcs) == 0 && len(build.Spec.Network.Subnets) > 0 {
		vpcs = []v1alpha1.VPC{{
			Name:     "vpc",
			Internet: true,
		}}
	}

	// Build effective subnet list, assigning orphan subnets to the first VPC.
	subnets := make([]v1alpha1.Subnet, len(build.Spec.Network.Subnets))
	copy(subnets, build.Spec.Network.Subnets)
	if len(vpcs) > 0 {
		for i := range subnets {
			if subnets[i].VPC == "" {
				subnets[i].VPC = vpcs[0].Name
			}
		}
	}

	// Create VPCs.
	for _, vpc := range vpcs {
		vpcName := buildVPCName(buildID, vpc.Name)
		internet := effectiveVPCInternet(build, vpc)
		if err := network.EnsureVPC(ctx, n.Client, vpcName, buildNS, internet, labels); err != nil {
			return v1alpha1.BuildPhaseNetworking, fmt.Errorf("ensuring VPC %s: %w", vpc.Name, err)
		}
		if !slices.Contains(build.Status.Network.VPCsCreated, vpcName) {
			build.Status.Network.VPCsCreated = append(build.Status.Network.VPCsCreated, vpcName)
		}
	}

	// Check all VPCs are ready before creating subnets.
	for _, vpc := range vpcs {
		vpcName := buildVPCName(buildID, vpc.Name)
		ready, err := network.IsVPCReady(ctx, n.Client, vpcName)
		if err != nil {
			return v1alpha1.BuildPhaseNetworking, fmt.Errorf("checking VPC %s readiness: %w", vpc.Name, err)
		}
		if !ready {
			logger.Info("Waiting for VPC to become ready", "vpc", vpcName)
			return v1alpha1.BuildPhaseNetworking, nil
		}
	}

	// Create OVN Subnets and their NADs.
	for _, subnet := range subnets {
		subnetName := buildSubnetName(buildID, subnet.Name)
		vpcName := buildVPCName(buildID, subnet.VPC)

		// Find if this subnet's VPC has internet (honoring buildOverrides).
		vpcInternet := false
		for _, v := range vpcs {
			if v.Name == subnet.VPC && effectiveVPCInternet(build, v) {
				vpcInternet = true
				break
			}
		}

		spec := network.SubnetSpec{
			Name: subnet.Name,
			VPC:  subnet.VPC,
			CIDR: subnet.CIDR,
			DHCP: subnet.DHCP == nil || *subnet.DHCP,
			DNS:  subnet.DNS,
		}
		// `unmanaged: true` is the user-facing knob for guest-served DHCP.
		// ApplyUnmanaged turns OVN's DHCP off, skips the gateway probe, and
		// relocates the OVN gateway router port off the guest gateway's
		// address (see its doc). OVN's IPAM stays enabled so every LSP can
		// get an IP - KubeOVN requires this - but the guest gateway's
		// address is excluded so it is never allocated. Honors buildOverrides:
		// a segment can be unmanaged in the base spec (so clones get it) yet
		// realized managed here so build-time provisioning has DHCP/relay.
		if effectiveSubnetUnmanaged(build, subnet.Name) {
			if err := spec.ApplyUnmanaged(); err != nil {
				return v1alpha1.BuildPhaseFailed, fmt.Errorf("subnet %q: %w", subnet.Name, err)
			}
		}

		if err := network.EnsureSubnet(ctx, n.Client, subnetName, vpcName, buildNS, spec, vpcInternet, labels); err != nil {
			return v1alpha1.BuildPhaseNetworking, fmt.Errorf("ensuring Subnet %s: %w", subnet.Name, err)
		}
		if !slices.Contains(build.Status.Network.SubnetsCreated, subnetName) {
			build.Status.Network.SubnetsCreated = append(build.Status.Network.SubnetsCreated, subnetName)
		}
	}

	// Check all subnets are ready.
	allReady := true
	for _, subnet := range subnets {
		subnetName := buildSubnetName(buildID, subnet.Name)
		ready, err := network.IsSubnetReady(ctx, n.Client, subnetName)
		if err != nil {
			return v1alpha1.BuildPhaseNetworking, fmt.Errorf("checking subnet %s readiness: %w", subnet.Name, err)
		}
		if !ready {
			allReady = false
		}
	}

	if !allReady {
		logger.Info("Waiting for network resources to become ready")
		return v1alpha1.BuildPhaseNetworking, nil
	}

	// Create VpcEgressGateways for internet-enabled VPCs (honoring buildOverrides).
	// This provides SNAT so VMs can reach the internet.
	for _, vpc := range vpcs {
		if !effectiveVPCInternet(build, vpc) {
			continue
		}
		vpcName := buildVPCName(buildID, vpc.Name)
		gwName := vpcName + "-egress"

		// The egress gateway pod auto-allocates an IP from each attached
		// subnet's OVN IPAM. Unmanaged subnets exclude their entire usable
		// range from IPAM (guest owns DHCP), so the gateway can't get an
		// IP there - skip them. If a VPC's only subnets are unmanaged,
		// the gateway can't be created; log and move on rather than wedge
		// the build in Networking forever.
		var vpcSubnetNames []string
		var firstSubnet string
		for _, subnet := range subnets {
			if subnet.VPC != vpc.Name || subnet.Unmanaged {
				continue
			}
			subnetName := buildSubnetName(buildID, subnet.Name)
			vpcSubnetNames = append(vpcSubnetNames, subnetName)
			if firstSubnet == "" {
				firstSubnet = subnetName
			}
		}
		if len(vpcSubnetNames) == 0 {
			logger.Info("VPC has internet=true but no managed subnets for egress gateway; skipping", "vpc", vpc.Name)
			continue
		}

		if err := network.EnsureEgressGateway(ctx, n.Client, gwName, buildNS, vpcName, firstSubnet, vpcSubnetNames, labels); err != nil {
			return v1alpha1.BuildPhaseNetworking, fmt.Errorf("ensuring egress gateway for VPC %s: %w", vpc.Name, err)
		}

		ready, internalIP, err := network.IsEgressGatewayReady(ctx, n.Client, gwName, buildNS)
		if err != nil {
			return v1alpha1.BuildPhaseNetworking, fmt.Errorf("checking egress gateway readiness: %w", err)
		}
		if !ready {
			logger.Info("Waiting for egress gateway to become ready", "gateway", gwName)
			return v1alpha1.BuildPhaseNetworking, nil
		}

		// Add default route to VPC pointing at egress gateway.
		if internalIP != "" {
			if err := network.EnsureVPCDefaultRoute(ctx, n.Client, vpcName, internalIP); err != nil {
				return v1alpha1.BuildPhaseNetworking, fmt.Errorf("setting VPC default route: %w", err)
			}
		}
	}

	// Register subnet CIDRs in the ovn40subnets ipset on all nodes.
	// KubeOVN only adds default overlay subnets to this ipset, but custom
	// VPC subnets need it too so that DHCP responses from OVN can traverse
	// the KubeVirt bridge (bridge-nf-call-iptables causes KUBE-FORWARD to
	// drop packets for unknown subnets).
	if n.RESTConfig != nil {
		for _, subnet := range subnets {
			cidr := subnet.CIDR
			if cidr == "" {
				cidr = "10.0.0.0/24" // default CIDR
			}
			if err := network.EnsureIPSetEntry(ctx, n.Client, n.RESTConfig, cidr); err != nil {
				logger.Error(err, "Failed to add subnet CIDR to ovn40subnets ipset", "cidr", cidr)
				// Non-fatal: DHCP may not work, but we can still proceed.
			}
		}
	}

	build.Status.Network.Ready = true
	logger.Info("All network resources are ready")
	return v1alpha1.BuildPhaseBuilding, nil
}

// buildVPCName returns the KubeOVN VPC resource name for a build.
func buildVPCName(buildID, name string) string {
	return network.VPCName(buildID, name)
}

// buildSubnetName returns the KubeOVN Subnet resource name for a build.
func buildSubnetName(buildID, name string) string {
	return network.SubnetName(buildID, name)
}

// subnetIsUnmanaged reports whether the build's spec marks the named subnet
// as unmanaged. Used by callers that can't talk to unmanaged subnets at the
// OVN-IPAM level (relay pod attaching to NADs, coordinator picking the SSH
// NIC). Unknown subnet names return false so we don't silently skip work.
func subnetIsUnmanaged(build *v1alpha1.VirtualMachineBuild, subnetName string) bool {
	if build == nil || build.Spec.Network == nil {
		return false
	}
	for _, s := range build.Spec.Network.Subnets {
		if s.Name == subnetName {
			return s.Unmanaged
		}
	}
	return false
}

// FirstManagedNIC returns the first NIC whose subnet is managed (OVN-IPAM
// available) for the BUILD phase, plus a bool indicating success. The relay pod
// and coordinator use this NIC as the SSH/HTTP path because unmanaged subnets
// exclude their usable range from IPAM and so cannot host the relay's
// logical-switch port. Uses the effective (buildOverride-aware) flag, so a
// segment overridden managed for the build counts as a valid provisioning NIC.
func FirstManagedNIC(build *v1alpha1.VirtualMachineBuild, nics []v1alpha1.VMNIC) (v1alpha1.VMNIC, bool) {
	for _, nic := range nics {
		if !effectiveSubnetUnmanaged(build, nic.Subnet) {
			return nic, true
		}
	}
	return v1alpha1.VMNIC{}, false
}

// VMAnnotationsForNICs generates the ruddervirt-compatible KubeOVN annotations
// for a VM's NIC assignments. These annotations are placed on the VMI pod.
func VMAnnotationsForNICs(buildID string, nics []v1alpha1.VMNIC) map[string]string {
	annotations := make(map[string]string)
	for _, nic := range nics {
		subnetName := buildSubnetName(buildID, nic.Subnet)
		prefix := fmt.Sprintf("ruddervirtvirt.io/net.vm.nic.%s", nic.Name)
		annotations[prefix+".subnet"] = subnetName
		if nic.IP != "" {
			annotations[prefix+".ip"] = nic.IP
		}
		if nic.MAC != "" {
			annotations[prefix+".mac"] = nic.MAC
		}
	}
	return annotations
}
