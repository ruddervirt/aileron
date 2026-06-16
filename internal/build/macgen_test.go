package build

import (
	"regexp"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

var macFormat = regexp.MustCompile(`^52:54:00:[0-9a-f]{2}:[0-9a-f]{2}:[0-9a-f]{2}$`)

func TestMaterializeMACs_AssignsMissingMACs(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{{
				Name: "builder",
				NICs: []v1alpha1.VMNIC{
					{Name: "nic0", Subnet: "s"},
					{Name: "nic1", Subnet: "s"},
				},
			}},
		},
	}

	if !MaterializeMACs(build) {
		t.Fatal("expected changed=true when assigning MACs")
	}

	for _, nic := range build.Spec.VMs[0].NICs {
		if !macFormat.MatchString(nic.MAC) {
			t.Errorf("nic %s has bad MAC %q", nic.Name, nic.MAC)
		}
	}
	if build.Spec.VMs[0].NICs[0].MAC == build.Spec.VMs[0].NICs[1].MAC {
		t.Error("expected distinct MACs for distinct NICs")
	}
}

func TestMaterializeMACs_PreservesUserSuppliedMACs(t *testing.T) {
	const userMAC = "aa:bb:cc:dd:ee:ff"
	build := &v1alpha1.VirtualMachineBuild{
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{{
				Name: "builder",
				NICs: []v1alpha1.VMNIC{
					{Name: "nic0", Subnet: "s", MAC: userMAC},
					{Name: "nic1", Subnet: "s"},
				},
			}},
		},
	}

	MaterializeMACs(build)

	if build.Spec.VMs[0].NICs[0].MAC != userMAC {
		t.Errorf("user MAC overwritten: got %q want %q", build.Spec.VMs[0].NICs[0].MAC, userMAC)
	}
	if build.Spec.VMs[0].NICs[1].MAC == "" {
		t.Error("missing MAC was not filled in")
	}
}

func TestMaterializeMACs_Idempotent(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{{
				Name: "builder",
				NICs: []v1alpha1.VMNIC{{Name: "nic0", Subnet: "s"}},
			}},
		},
	}

	if !MaterializeMACs(build) {
		t.Fatal("first pass should report changed=true")
	}
	mac := build.Spec.VMs[0].NICs[0].MAC

	if MaterializeMACs(build) {
		t.Error("second pass should report changed=false")
	}
	if build.Spec.VMs[0].NICs[0].MAC != mac {
		t.Errorf("MAC changed on re-materialize: got %q want %q", build.Spec.VMs[0].NICs[0].MAC, mac)
	}
}

func TestMaterializeMACs_NoChangeWhenAllMACsPresent(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{{
				Name: "builder",
				NICs: []v1alpha1.VMNIC{{Name: "nic0", Subnet: "s", MAC: "52:54:00:00:00:01"}},
			}},
		},
	}
	if MaterializeMACs(build) {
		t.Error("expected changed=false when no NIC needs a MAC")
	}
}

func TestMaterializeMACs_CoversBuildOverrides(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{{
				Name: "builder",
				NICs: []v1alpha1.VMNIC{{Name: "nic0", Subnet: "s"}},
			}},
			BuildOverrides: &v1alpha1.BuildOverrides{
				VMs: []v1alpha1.BuildVMOverride{{
					Name: "builder",
					NICs: []v1alpha1.VMNIC{{Name: "prov", Subnet: "prov"}},
				}},
			},
		},
	}

	MaterializeMACs(build)

	if build.Spec.VMs[0].NICs[0].MAC == "" {
		t.Error("base NIC missing MAC")
	}
	if build.Spec.BuildOverrides.VMs[0].NICs[0].MAC == "" {
		t.Error("override NIC missing MAC")
	}
	if build.Spec.VMs[0].NICs[0].MAC == build.Spec.BuildOverrides.VMs[0].NICs[0].MAC {
		t.Error("base and override NICs got the same MAC (collision)")
	}
}
