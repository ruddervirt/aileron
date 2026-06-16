# Research memo: "unmanaged" networks as a bare L2 cable, within kube-ovn

**Date:** 2026-05-31
**Cluster:** rudderusa (KubeOVN v1.15.2, KubeVirt v1.8.2)
**Requirement:** An `unmanaged` subnet must behave like plugging in a bare network
cable: a pure L2 broadcast domain with **no fabric IPAM and no fabric DHCP**, so a
guest gateway VM (pfSense) owns DHCP/routing for the segment. Constraint from the
team: **the solution must stay in kube-ovn/OVN** (not a separate Linux-bridge CNI).

Stated reasons OVN is required: **cross-node L2**, **single-stack / iptables-bypass
consistency**, **clean resource lifecycle**. (VPC isolation is explicitly *not* a
requirement — but per-segment L2 isolation between builds still is.)

---

## 1. The root incompatibility

Two facts collide and define the whole problem:

1. **KubeVirt `bridge` binding starts its in-pod DHCP server (`SingleClientDHCPServer`)
   if and only if the CNI assigned the pod interface an IPv4.** KubeVirt docs:
   *"KubeVirt's DHCP server is used only in the case that the pod's interface did get
   an IP ... If the pod just gets an L2 network, KubeVirt's DHCP server is not used,
   and the VM gets an IP from the L2 network it is connected to (if DHCP exists there)."*
   There is no built-in "bridge without DHCP" knob.

2. **kube-ovn's `.ovn` overlay always does IPAM per port and configures the pod NIC's
   IP.** There is no native L2-only / IP-less mode for a `kube-ovn`-type port
   (confirmed across the v1.15 subnet, DHCP, multi-NIC, and KubeVirt docs). kube-ovn's
   *own* OVN DHCP responder exists only "because KubeVirt's built-in DHCP does not work"
   on **SR-IOV/DPDK** — i.e. for ordinary bridge binding, virt-launcher serving the
   kube-ovn IP *is* the designed path.

**Consequence:** on kube-ovn overlay + bridge binding the VM **always** receives an IP
from virt-launcher's `SingleClientDHCPServer`, so pfSense's DHCP can never win. This is
exactly the `192.168.0.x` observed on build `ns-iidli1pi06zryxj` (VMs `one`/`two` got
the kube-ovn-IPAM addresses `.3/.4`, never pfSense's `192.168.1.x`). It is structural,
not a bug in the `unmanaged` flag — which *does* correctly disable OVN's own DHCP
responder (`enableDHCP:false`, no `dhcpV4Options` on the LSP).

### Verified on the live build
- `lan-subnet`: `enableDHCP` empty, no `dhcpV4Options` -> OVN DHCP off (good).
- LSPs for `one`/`two`: `addresses = "<mac> 192.168.0.x"`, **no `dhcpv4_options`**.
- pod annotation `...lan-subnet-nad...ovn.kubernetes.io/ip_address: 192.168.0.3`.
- VMI interface binding: `"bridge": {}`; VMI reported IP == pod IPAM IP.
- virt-launcher log: `"Starting SingleClientDHCPServer"`.

## 2. Approaches ruled out (with evidence)

- **Exclude the whole CIDR from IPAM** (the design's never-wired-up intent in
  `internal/build/network.go` — `SubnetSpec.ExcludeIPs` is defined but never
  populated): kube-ovn then has no address to allocate, so **port creation fails**;
  it does not yield an IP-less port.
- **Omit an `ipam` stanza in a kube-ovn NAD**: unsupported/undocumented for the `.ovn`
  provider; kube-ovn NADs carry no `ipam` field — the Subnet is the IPAM unit.
- **A pod annotation to skip IP / DHCP per-port**: does not exist. kube-ovn issue #5848
  documents there is not even a way to control secondary-LSP addresses.
- **Old `bridge`-CNI path** (removed by commit `594296f "ovn for unmanaged"`;
  `aibr-*` host bridges, `isGateway:false`, `ipMasq:false`, no IPAM): this is the
  textbook bare-cable approach and matches KubeVirt's own guidance, **but it is
  node-local** and therefore fails the cross-node-L2 requirement. Correctly abandoned
  for that reason.

## 3. Options that exist within OVN

### Option A — Keep kube-ovn overlay; suppress virt-launcher DHCP via a KubeVirt network binding plugin  (BEST FIT)
Keep the `.ovn` logical switch (cross-node L2 ✓, OVS-only consistency ✓, existing
subnet teardown lifecycle ✓, per-build logical-switch L2 isolation ✓). Replace the
default `bridge` binding on unmanaged NICs with a **network binding plugin**
(KubeVirt v1.1+, supported on v1.8.2) that attaches the tap at pure L2 and does **not**
run the DHCP server. The guest then DHCPs from pfSense across the OVN switch; the
kube-ovn-assigned IP becomes inert.
- **Meets the requirement:** yes — true bare-cable behavior, fully on OVN, all three
  stated OVN reasons satisfied.
- **Cost / risk:** must author, containerize, and register a binding plugin
  (`spec.configuration.network.binding` in the KubeVirt CR) — effectively the in-tree
  bridge binding minus the IP/DHCP config step. New image to maintain; per-NIC wiring
  in `internal/build/vm.go` + clone `RewireVMNetworks` to select the plugin for
  unmanaged NICs. The `Sidecar` gate is already on, but a domain-XML sidecar **cannot**
  suppress the DHCP server (it runs in the network-setup phase) — it must be a binding
  plugin. KubeVirt's `passt` plugin is the reference implementation to model from.

### Option B — kube-ovn underlay / VLAN subnet for unmanaged segments  (POOR FIT)
Underlay mode (OVS bridges a provider NIC onto a VLAN), `enableDHCP:false` +
`excludeIps`, pfSense and clients sharing the physical VLAN.
- **Cross-node L2:** yes (via the physical VLAN).
- **Costs that conflict with your priorities:** needs a physical provider network/VLAN
  provisioned per segment (hurts clean lifecycle + single-stack consistency); underlay
  subnets **cannot be VPC-isolated** (not needed by you, but worth noting); pfSense must
  live on that underlay VLAN. Heavy and awkward for ephemeral per-build segments.

### Option C — Let OVN serve DHCP (`enableDHCP:true`), pfSense only routes  (DOES NOT MEET REQUIREMENT)
Technically cleanest, but the requirement is that the **gateway VM owns DHCP** (lab
scenario; students configure pfSense scopes/options). Listed for completeness only.

## 4. Recommendation

**Option A.** It is the only path that keeps cross-node L2, single-stack/iptables-bypass
consistency, and the existing OVN subnet lifecycle **and** delivers the bare-cable
requirement. The cost is real engineering: a KubeVirt L2-no-DHCP binding plugin plus
NIC-selection wiring in the build and clone network paths.

**Open upstream context worth tracking:** a KubeVirt-native "bridge binding without
DHCP" flag would make Option A trivial; absent that, the binding plugin is the
supported mechanism. kube-ovn `managedTap` is *not* a shortcut — it still binds the IP
to the VM lifecycle (still serves an address).

### Code touch-points if Option A proceeds (from the current tree)
- `api/v1alpha1/network_types.go` — `Subnet.Unmanaged` doc.
- `internal/network/subnet.go::EnsureSubnet` — keep `.ovn` subnet; no IPAM change
  needed (port keeps an inert IP).
- `internal/build/vm.go` (~L257) and `internal/clone/network.go::RewireVMNetworks` —
  select the L2-no-DHCP binding (instead of `"bridge": {}`) for NICs on unmanaged
  subnets; stop emitting `routes`/gateway annotations for those NICs.
- Relay/coordinator paths already require a managed NIC for unmanaged-only VMs
  (`FirstManagedNIC`, `coordinator_handler.go`) — unchanged.
- KubeVirt CR: register the binding plugin once at deploy time.

## Addendum (2026-06-05): the OVN gateway squat, and why logicalGateway can't fix it

Option A (l2bridge/managedTap binding) shipped and works — virt-launcher serves
no DHCP on unmanaged NICs and guests lease from pfSense. Live clones then
exposed a SECOND structural collision: **kube-ovn attaches a gateway
logical-router port (LRP) to every overlay subnet at `spec.gateway`**, which
defaults to the first usable IP — the same `192.168.1.1` pfSense uses. The LRP
answers ARP for `.1` and intercepts traffic to it (observed: client DNS to its
gateway answered with `ICMP udp port 53 unreachable` from the LRP MAC), so
clients lease from pfSense but can never reach it.

`spec.logicalGateway` is NOT the fix: in kube-ovn release-1.15 it only applies
to underlay/VLAN subnets ("overlay subnet, should add lrp, lrp ip is subnet
gw" is unconditional in pkg/controller/subnet.go). Deleting the LRP by hand
(hack/fix-unmanaged-gateway.sh) works only until the next subnet reconcile —
a kube-ovn-controller restart resurrects it.

**Durable fix (implemented):** `SubnetSpec.ApplyUnmanaged` relocates the OVN
gateway to the SECOND-TO-LAST usable IP of the CIDR (e.g. `.253` in a /24) at
subnet creation, and excludes the first usable IP from OVN IPAM so it stays
reserved for the guest gateway. The mandatory LRP then exists harmlessly;
constraints: CIDR must be /29 or wider, and the guest's DHCP pool must not
hand out the relocated address.

## Sources
- KubeVirt interfaces & networks: https://kubevirt.io/user-guide/network/interfaces_and_networks/
- KubeVirt bridge CNI / L2 DHCP behavior: https://kubevirt.io/2020/Multiple-Network-Attachments-with-bridge-CNI.html
- kube-ovn Subnet config (v1.15): https://kubeovn.github.io/docs/v1.15.x/en/guide/subnet/
- kube-ovn DHCP (SR-IOV/DPDK rationale): https://kubeovn.github.io/docs/v1.12.x/en/advance/dhcp/
- kube-ovn multi-NIC / provider semantics: https://kubeovn.github.io/docs/v1.14.x/en/advance/multi-nic/
- kube-ovn KubeVirt fixed addresses: https://kubeovn.github.io/docs/v1.14.x/en/kubevirt/static-ip/
- kube-ovn underlay: https://kubeovn.github.io/docs/v1.13.x/en/start/underlay/
- kube-ovn issue #5848 (no per-NIC secondary-LSP address control): https://github.com/kubeovn/kube-ovn/issues/5848
