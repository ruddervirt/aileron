package build

import (
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// effectiveVPCInternet returns whether a VPC has internet access during the
// build phase, honoring any buildOverrides. The base spec.network.vpcs[].internet
// value is what clones inherit — this helper is only used when building
// the ephemeral build infrastructure.
func effectiveVPCInternet(build *v1alpha1.VirtualMachineBuild, vpc v1alpha1.VPC) bool {
	if build.Spec.BuildOverrides == nil {
		return vpc.Internet
	}
	for _, o := range build.Spec.BuildOverrides.VPCs {
		if o.Name == vpc.Name && o.Internet != nil {
			return *o.Internet
		}
	}
	return vpc.Internet
}

// effectiveSubnetUnmanaged reports whether a subnet is unmanaged for the BUILD
// phase, honoring any buildOverrides. The base spec.network.subnets[].unmanaged
// value is what the template captures and clones inherit — this helper is only
// used when realizing the ephemeral build network (subnet creation, VM binding,
// relay/coordinator reachability). A buildOverride lets a segment that is
// unmanaged in the base spec be built as managed: an unmanaged segment is dark
// during its own build (no OVN/virt-launcher DHCP, and the guest gateway that
// will serve it doesn't exist yet), so provisioning needs the managed path.
func effectiveSubnetUnmanaged(build *v1alpha1.VirtualMachineBuild, subnetName string) bool {
	base := subnetIsUnmanaged(build, subnetName)
	if build.Spec.BuildOverrides == nil {
		return base
	}
	for _, o := range build.Spec.BuildOverrides.Subnets {
		if o.Name == subnetName && o.Unmanaged != nil {
			return *o.Unmanaged
		}
	}
	return base
}

// effectiveVMNICs returns the VM's NIC list during the build phase, honoring
// any buildOverrides. The base spec.vms[].nics list is what clones inherit —
// this helper is only used when wiring up the ephemeral build VM. Replacement
// semantics: a non-empty override list fully replaces the base list, except
// that an override NIC sharing a name with a base NIC keeps the base NIC's
// identity fields (mac, slot, model). Guest OS state binds to those fields —
// Windows keys DHCP reservations/leases to the MAC and adapter config to the
// PCI path — and clones run with the base values, so a build that deviates
// bakes guest state no clone can ever match.
func effectiveVMNICs(build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) []v1alpha1.VMNIC {
	if build.Spec.BuildOverrides == nil {
		return vmSpec.NICs
	}
	for _, o := range build.Spec.BuildOverrides.VMs {
		if o.Name == vmSpec.Name && len(o.NICs) > 0 {
			return inheritNICIdentity(vmSpec, o.NICs)
		}
	}
	return vmSpec.NICs
}

// inheritNICIdentity returns the override NIC list with identity fields
// (mac, slot, model) taken from the same-name base-spec NIC wherever the
// base value is set. Override-only NICs (no base counterpart, e.g. a
// build-time provisioning NIC) pass through unchanged, as do fields the
// base spec leaves empty.
func inheritNICIdentity(vmSpec *v1alpha1.BuildVM, overrides []v1alpha1.VMNIC) []v1alpha1.VMNIC {
	baseByName := make(map[string]*v1alpha1.VMNIC, len(vmSpec.NICs))
	for i := range vmSpec.NICs {
		baseByName[vmSpec.NICs[i].Name] = &vmSpec.NICs[i]
	}
	result := make([]v1alpha1.VMNIC, len(overrides))
	copy(result, overrides)
	for i := range result {
		base, ok := baseByName[result[i].Name]
		if !ok {
			continue
		}
		nic := &result[i]
		if base.MAC != "" && nic.MAC != base.MAC {
			logNICIdentityOverride(vmSpec.Name, nic.Name, "mac", nic.MAC, base.MAC)
			nic.MAC = base.MAC
		}
		if base.Slot != 0 && nic.Slot != base.Slot {
			logNICIdentityOverride(vmSpec.Name, nic.Name, "slot", nic.Slot, base.Slot)
			nic.Slot = base.Slot
		}
		if base.Model != "" && nic.Model != base.Model {
			logNICIdentityOverride(vmSpec.Name, nic.Name, "model", nic.Model, base.Model)
			nic.Model = base.Model
		}
	}
	return result
}

// logNICIdentityOverride notes a corrected identity-field mismatch, but only
// when the override actually authored a conflicting value — inheriting into
// an empty field is the documented default and not worth log noise.
func logNICIdentityOverride(vmName, nicName, field string, overrideVal, baseVal any) {
	if overrideVal == "" || overrideVal == 0 {
		return
	}
	logf.Log.WithName("build-overrides").Info(
		"Ignoring buildOverride NIC identity field; base spec value wins so guest state matches clones",
		"vm", vmName, "nic", nicName, "field", field, "override", overrideVal, "base", baseVal)
}

// effectiveVMResources returns the VM's compute resources during the build
// phase, honoring any buildOverrides. The base spec.vms[].resources value is
// what clones inherit — this helper is only used when booting the ephemeral
// build VM.
func effectiveVMResources(build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) v1alpha1.BuildResources {
	result := vmSpec.Resources
	if build.Spec.BuildOverrides == nil {
		return result
	}
	for _, o := range build.Spec.BuildOverrides.VMs {
		if o.Name != vmSpec.Name || o.Resources == nil {
			continue
		}
		if !o.Resources.CPU.IsZero() {
			result.CPU = o.Resources.CPU
		}
		if !o.Resources.Memory.IsZero() {
			result.Memory = o.Resources.Memory
		}
	}
	return result
}
