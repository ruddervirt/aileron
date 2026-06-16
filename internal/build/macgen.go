package build

import (
	"crypto/rand"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

// macOUI is the QEMU/KubeVirt-assigned OUI (locally administered,
// unicast). Generating MACs in this range avoids collisions with NIC
// vendors and matches what KubeVirt itself would pick if it auto-assigned.
const macOUI = "52:54:00"

// MaterializeMACs assigns a stable MAC to every NIC in the build spec
// that doesn't already have one, so the assignment round-trips through
// the template-VM topology annotation to clones. Without this, the build
// VM gets a libvirt-random MAC at boot that's never persisted, so the
// cloned VM ends up with a different MAC and the captured Windows image
// treats every NIC as new hardware (losing per-adapter state).
//
// MACs are written to both spec.vms[].nics[].mac (inherited by clones)
// and spec.buildOverrides.vms[].nics[].mac (build-only NICs). Returns
// true if any MAC was assigned.
//
// The function is idempotent: NICs that already have a MAC are left
// untouched, so re-running after a partial spec update never flips an
// existing value.
func MaterializeMACs(vmBuild *v1alpha1.VirtualMachineBuild) bool {
	used := map[string]bool{}
	collectExistingMACs(vmBuild, used)

	changed := false
	for i := range vmBuild.Spec.VMs {
		for j := range vmBuild.Spec.VMs[i].NICs {
			nic := &vmBuild.Spec.VMs[i].NICs[j]
			if nic.MAC != "" {
				continue
			}
			nic.MAC = freshMAC(used)
			changed = true
		}
	}
	if vmBuild.Spec.BuildOverrides != nil {
		for i := range vmBuild.Spec.BuildOverrides.VMs {
			for j := range vmBuild.Spec.BuildOverrides.VMs[i].NICs {
				nic := &vmBuild.Spec.BuildOverrides.VMs[i].NICs[j]
				if nic.MAC != "" {
					continue
				}
				nic.MAC = freshMAC(used)
				changed = true
			}
		}
	}
	return changed
}

func collectExistingMACs(vmBuild *v1alpha1.VirtualMachineBuild, used map[string]bool) {
	for _, vm := range vmBuild.Spec.VMs {
		for _, nic := range vm.NICs {
			if nic.MAC != "" {
				used[nic.MAC] = true
			}
		}
	}
	if vmBuild.Spec.BuildOverrides == nil {
		return
	}
	for _, vm := range vmBuild.Spec.BuildOverrides.VMs {
		for _, nic := range vm.NICs {
			if nic.MAC != "" {
				used[nic.MAC] = true
			}
		}
	}
}

// freshMAC returns a MAC in the macOUI range that doesn't collide with
// any value in `used`, marking the returned MAC as used so subsequent
// calls don't reissue it. The 24-bit suffix gives ~16M values per build,
// so the retry loop is effectively a no-op even with many NICs.
func freshMAC(used map[string]bool) string {
	for {
		var b [3]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Errorf("mac generation: %w", err))
		}
		m := fmt.Sprintf("%s:%02x:%02x:%02x", macOUI, b[0], b[1], b[2])
		if !used[m] {
			used[m] = true
			return m
		}
	}
}
