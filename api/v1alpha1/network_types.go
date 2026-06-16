/*
Copyright 2026.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/

package v1alpha1

// =============================================================================
// Network: VPCs, Subnets, Per-VM NICs (mirrors ruddervirt annotation model)
// =============================================================================

// Network defines the KubeOVN network topology for an operation.
type Network struct {
	// vpcs defines the virtual private clouds for the operation.
	// +optional
	VPCs []VPC `json:"vpcs,omitempty"`

	// subnets defines the subnets within VPCs.
	// +optional
	Subnets []Subnet `json:"subnets,omitempty"`
}

// VPC defines a KubeOVN VPC. Most operations don't need to declare VPCs
// explicitly — one is auto-created per operation. Use this only when you need
// multiple isolated VPCs within a single operation.
type VPC struct {
	// name is the VPC identifier, referenced by subnets.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// internet enables NAT egress and public DNS (8.8.8.8, 1.1.1.1) for this VPC.
	// Overridden to false when spec.internet is false.
	// +kubebuilder:default=false
	// +optional
	Internet bool `json:"internet,omitempty"`
}

// Subnet defines a KubeOVN subnet within a VPC.
type Subnet struct {
	// name is the subnet identifier, referenced by VM NIC assignments.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// vpc is the name of the VPC this subnet belongs to.
	// If omitted, the subnet is placed in the auto-created VPC.
	// +optional
	VPC string `json:"vpc,omitempty"`

	// cidr is the IPv4 CIDR block (e.g. "10.0.1.0/24"). Required.
	// +required
	// +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
	CIDR string `json:"cidr"`

	// dhcp enables KubeOVN DHCP on this subnet. Defaults to true.
	// Ignored when unmanaged is true (the guest owns DHCP).
	// +kubebuilder:default=true
	// +optional
	DHCP *bool `json:"dhcp,omitempty"`

	// dns overrides DNS server addresses for DHCP (e.g. "10.0.1.1" or "10.0.1.1,10.0.1.2").
	// When spec.internet is true the default is public DNS; otherwise there is no default.
	// +optional
	DNS string `json:"dns,omitempty"`

	// unmanaged turns this subnet into a bare L2 segment owned by a guest
	// gateway VM (e.g. pfSense or a Windows DC serving DHCP). The operator
	// still realizes the subnet as an OVN logical switch - that's what
	// gives us cross-node L2, VPC isolation, and OVS forwarding that
	// bypasses iptables - but: OVN's DHCP is turned off, NICs attach via
	// the l2bridge (managedTap) binding so virt-launcher serves no DHCP
	// either, the gateway health check is skipped, and KubeOVN's mandatory
	// gateway router port is relocated to the SECOND-TO-LAST usable IP of
	// the CIDR (e.g. .253 in a /24) so it cannot shadow the guest gateway
	// at the first usable IP. OVN IPAM stays on (KubeOVN requires every
	// port to have an IP) but the allocations are inert; the first usable
	// IP is excluded so the guest gateway's address is never handed out.
	//
	// Constraints: the CIDR must be /29 or wider; the guest's DHCP pool
	// must not include the relocated OVN gateway (second-to-last usable
	// IP); declare the CIDR equal to the range the guest gateway actually
	// serves so the ovn40subnets ipset entry matches real traffic; dns has
	// no effect on unmanaged subnets (the guest provides it).
	// +optional
	Unmanaged bool `json:"unmanaged,omitempty"`
}

// VMNIC defines a NIC assignment for a VM, connecting it to a subnet.
type VMNIC struct {
	// name is a label for this NIC. It's used to key the VMI interface
	// and network entries against each other but does not control the
	// in-guest interface name or the PCI placement; pin slot for that.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// subnet is the name of the subnet to connect to.
	// +required
	Subnet string `json:"subnet"`

	// slot pins the NIC to a specific PCI slot on the VM, so the same
	// logical NIC keeps the same PCI address across the base build and
	// every downstream build that reuses the image. Without this, the
	// guest sees "new" hardware whenever a downstream spec reorders the
	// NIC list, and persisted per-adapter state (Windows adapter rename,
	// static IP, DHCPServer binding, AD DNS binding) lands on the wrong
	// interface. The PCI address is computed from the slot by Aileron.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9
	Slot int `json:"slot,omitempty"`

	// ip is an optional static IP address. If omitted on a DHCP subnet, auto-assigned.
	// +optional
	IP string `json:"ip,omitempty"`

	// mac is an optional static MAC address.
	// +optional
	MAC string `json:"mac,omitempty"`

	// model is the libvirt NIC model exposed to the guest. Defaults to
	// e1000 because every modern guest OS ships an in-box driver for it,
	// avoiding the need to side-load netkvm during Windows install or to
	// rely on virtio support in older guests. Switch to virtio for higher
	// throughput once the guest has the netkvm/virtio_net driver loaded.
	// +optional
	// +kubebuilder:validation:Enum=virtio;e1000;e1000e;rtl8139;pcnet;ne2k_pci
	// +kubebuilder:default="e1000"
	Model string `json:"model,omitempty"`
}
