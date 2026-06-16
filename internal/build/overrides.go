package build

import (
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
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
// semantics: a non-empty override list fully replaces the base list.
func effectiveVMNICs(build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) []v1alpha1.VMNIC {
	if build.Spec.BuildOverrides == nil {
		return vmSpec.NICs
	}
	for _, o := range build.Spec.BuildOverrides.VMs {
		if o.Name == vmSpec.Name && len(o.NICs) > 0 {
			return o.NICs
		}
	}
	return vmSpec.NICs
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
		if o.Resources.CPU != 0 {
			result.CPU = o.Resources.CPU
		}
		if !o.Resources.Memory.IsZero() {
			result.Memory = o.Resources.Memory
		}
	}
	return result
}
