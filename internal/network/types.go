package network

// VPCSpec defines a VPC to create.
type VPCSpec struct {
	// Name is the logical VPC name (will be prefixed for uniqueness).
	Name string
	// Internet enables NAT egress for this VPC.
	Internet bool
}

// SubnetSpec defines a subnet to create within a VPC.
type SubnetSpec struct {
	// Name is the logical subnet name.
	Name string
	// VPC is the logical VPC name this subnet belongs to.
	VPC string
	// CIDR is the IPv4 CIDR block.
	CIDR string
	// DHCP enables OVN DHCP on this subnet. Set false when a guest serves
	// DHCP for the L2 segment instead.
	DHCP bool
	// DNS overrides DNS server addresses.
	DNS string
	// ExcludeIPs are IP addresses or ranges OVN IPAM must not allocate.
	// KubeOVN range syntax (e.g. "10.0.1.100..10.0.1.200").
	ExcludeIPs []string
	// DisableGatewayCheck skips OVN's ICMP gateway health check.
	DisableGatewayCheck bool
	// Gateway overrides the KubeOVN subnet gateway IP. Empty means KubeOVN's
	// default (the first usable IP of the CIDR). Set by ApplyUnmanaged to keep
	// OVN's mandatory gateway router port off the guest gateway's address.
	Gateway string
}

// ApplyUnmanaged translates the user-facing `unmanaged` flag into the
// underlying OVN config for a bare-L2 segment where a guest VM (e.g. pfSense)
// owns DHCP and routing:
//
//   - OVN's DHCP responder is turned off so the guest's server is the only one
//     answering on the switch.
//   - The gateway health probe is skipped (the OVN gateway is symbolic here).
//   - The OVN gateway is relocated to the second-to-last usable IP of the
//     CIDR. KubeOVN unconditionally attaches a gateway logical-router port to
//     every overlay subnet at spec.gateway (spec.logicalGateway only applies
//     to underlay/VLAN subnets), and that port answers ARP for — and
//     intercepts traffic to — its address. Left at the default first-usable
//     IP it squats the conventional guest-gateway address (e.g. pfSense's
//     192.168.1.1), so clients lease from the guest but can never reach it.
//     Relocating the port (e.g. to .253 in a /24) leaves it harmless; the
//     guest's DHCP pool must simply not hand that address out.
//   - The first usable IP is excluded from OVN IPAM so no VM port is ever
//     allocated the guest gateway's address. (KubeOVN auto-excludes the
//     relocated gateway itself.)
//
// Returns an error when the CIDR is too small to keep the guest gateway and
// the relocated OVN gateway distinct (narrower than /29).
func (s *SubnetSpec) ApplyUnmanaged() error {
	gw, err := UnmanagedGateway(s.CIDR)
	if err != nil {
		return err
	}
	s.DHCP = false
	s.DisableGatewayCheck = true
	s.Gateway = gw
	s.ExcludeIPs = append(s.ExcludeIPs, SubnetGateway(s.CIDR))
	return nil
}

// NetworkTopology defines the full network spec for a build or clone operation.
type NetworkTopology struct {
	// Prefix is used for all resource names (e.g. "aileron-mybuild").
	Prefix string
	// Namespace is the target namespace for NADs.
	Namespace string
	// VPCs to create.
	VPCs []VPCSpec
	// Subnets to create.
	Subnets []SubnetSpec
	// InternetEnabled is the global internet flag.
	InternetEnabled bool
	// OwnerLabels are labels applied to all created resources.
	OwnerLabels map[string]string
}

// VPCName creates a unique name for a KubeOVN VPC.
// Suffixed with "-vpc" so it can never collide with a subnet of the same
// logical name (KubeOVN rejects a subnet and VPC that share a k8s name).
func VPCName(prefix, name string) string {
	return prefix + "-" + name + "-vpc"
}

// SubnetName creates a unique name for a KubeOVN subnet.
// Suffixed with "-subnet" to avoid collisions with VPCs.
func SubnetName(prefix, name string) string {
	return prefix + "-" + name + "-subnet"
}

// ResourceName creates a unique name for a generic network resource.
// Prefer VPCName or SubnetName for VPCs and subnets to avoid name collisions.
func ResourceName(prefix, name string) string {
	return prefix + "-" + name
}
