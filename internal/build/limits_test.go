package build

import (
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func makeSpec(vms ...v1alpha1.BuildVM) *v1alpha1.VirtualMachineBuildSpec {
	return &v1alpha1.VirtualMachineBuildSpec{VMs: vms}
}

func makeVM(name string, cpu int32, memory string, disks ...v1alpha1.BuildDisk) v1alpha1.BuildVM {
	return v1alpha1.BuildVM{
		Name: name,
		Resources: v1alpha1.BuildResources{
			CPU:    *resource.NewQuantity(int64(cpu), resource.DecimalSI),
			Memory: resource.MustParse(memory),
		},
		Disks: disks,
	}
}

func makeDisk(name, size string) v1alpha1.BuildDisk {
	return v1alpha1.BuildDisk{Name: name, Size: resource.MustParse(size)}
}

func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestEnforceLimits_NilLimits(t *testing.T) {
	var bl *BuildLimits
	changed, msgs, err := bl.EnforceLimits(makeSpec(makeVM("a", 8, "16Gi")))
	if err != nil || changed || len(msgs) > 0 {
		t.Fatalf("nil limits should be no-op: changed=%v msgs=%v err=%v", changed, msgs, err)
	}
}

func TestEnforceLimits_NoLimitsConfigured(t *testing.T) {
	bl := &BuildLimits{} // all zero/nil
	spec := makeSpec(makeVM("a", 64, "256Gi", makeDisk("root", "500Gi")))
	changed, msgs, err := bl.EnforceLimits(spec)
	if err != nil || changed || len(msgs) > 0 {
		t.Fatalf("zero limits should be no-op: changed=%v msgs=%v err=%v", changed, msgs, err)
	}
}

func TestEnforceLimits_CPUClamp(t *testing.T) {
	bl := &BuildLimits{MaxCPU: qty("4")}
	spec := makeSpec(makeVM("server", 8, "4Gi"))
	changed, msgs, err := bl.EnforceLimits(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected spec change")
	}
	if spec.VMs[0].Resources.CPU.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("CPU = %s, want 4", spec.VMs[0].Resources.CPU.String())
	}
	if len(msgs) != 1 {
		t.Errorf("msgs = %v, want 1 message", msgs)
	}
}

func TestEnforceLimits_CPUUnderLimit(t *testing.T) {
	bl := &BuildLimits{MaxCPU: qty("8")}
	spec := makeSpec(makeVM("server", 4, "4Gi"))
	changed, _, err := bl.EnforceLimits(spec)
	if err != nil || changed {
		t.Fatalf("should be no-op: changed=%v err=%v", changed, err)
	}
}

func TestEnforceLimits_MemoryClamp(t *testing.T) {
	bl := &BuildLimits{MaxMemory: qty("8Gi")}
	spec := makeSpec(makeVM("server", 2, "16Gi"))
	changed, msgs, err := bl.EnforceLimits(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected spec change")
	}
	if spec.VMs[0].Resources.Memory.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("Memory = %s, want 8Gi", spec.VMs[0].Resources.Memory.String())
	}
	if len(msgs) != 1 {
		t.Errorf("msgs = %v, want 1 message", msgs)
	}
}

func TestEnforceLimits_DiskSizeClamp(t *testing.T) {
	bl := &BuildLimits{MaxDiskSize: qty("50Gi")}
	spec := makeSpec(makeVM("server", 2, "4Gi", makeDisk("root", "100Gi"), makeDisk("data", "30Gi")))
	changed, msgs, err := bl.EnforceLimits(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected spec change")
	}
	if spec.VMs[0].Disks[0].Size.Cmp(resource.MustParse("50Gi")) != 0 {
		t.Errorf("root disk = %s, want 50Gi", spec.VMs[0].Disks[0].Size.String())
	}
	// data disk should be untouched (under limit)
	if spec.VMs[0].Disks[1].Size.Cmp(resource.MustParse("30Gi")) != 0 {
		t.Errorf("data disk = %s, want 30Gi (unchanged)", spec.VMs[0].Disks[1].Size.String())
	}
	if len(msgs) != 1 {
		t.Errorf("msgs = %v, want 1 message (only root clamped)", msgs)
	}
}

func TestEnforceLimits_DiskCountFail(t *testing.T) {
	bl := &BuildLimits{MaxDiskCount: 1}
	spec := makeSpec(makeVM("server", 2, "4Gi", makeDisk("root", "25Gi"), makeDisk("data", "25Gi")))
	_, _, err := bl.EnforceLimits(spec)
	if err == nil {
		t.Fatal("expected error for disk count violation")
	}
}

func TestEnforceLimits_VMCountFail(t *testing.T) {
	bl := &BuildLimits{MaxVMCount: 1}
	spec := makeSpec(makeVM("a", 2, "4Gi"), makeVM("b", 2, "4Gi"))
	_, _, err := bl.EnforceLimits(spec)
	if err == nil {
		t.Fatal("expected error for VM count violation")
	}
}

func TestEnforceLimits_MultipleVMs(t *testing.T) {
	bl := &BuildLimits{MaxCPU: qty("4"), MaxMemory: qty("8Gi")}
	spec := makeSpec(
		makeVM("small", 2, "4Gi"),   // under limits
		makeVM("big", 16, "32Gi"),   // both clamped
		makeVM("medium", 4, "12Gi"), // only memory clamped
	)
	changed, msgs, err := bl.EnforceLimits(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected spec change")
	}
	if spec.VMs[0].Resources.CPU.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("small CPU changed unexpectedly: %s", spec.VMs[0].Resources.CPU.String())
	}
	if spec.VMs[1].Resources.CPU.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("big CPU = %s, want 4", spec.VMs[1].Resources.CPU.String())
	}
	if spec.VMs[2].Resources.CPU.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("medium CPU = %s, want 4 (unchanged)", spec.VMs[2].Resources.CPU.String())
	}
	if spec.VMs[2].Resources.Memory.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("medium Memory = %s, want 8Gi", spec.VMs[2].Resources.Memory.String())
	}
	// big VM: 2 clamps (CPU + memory), medium VM: 1 clamp (memory) = 3 total
	if len(msgs) != 3 {
		t.Errorf("msgs count = %d, want 3: %v", len(msgs), msgs)
	}
}

func TestLoadBuildLimits(t *testing.T) {
	t.Setenv("BUILD_LIMIT_MAX_CPU", "4")
	t.Setenv("BUILD_LIMIT_MAX_MEMORY", "8Gi")
	t.Setenv("BUILD_LIMIT_MAX_DISK_SIZE", "100Gi")
	t.Setenv("BUILD_LIMIT_MAX_DISK_COUNT", "2")
	t.Setenv("BUILD_LIMIT_MAX_VM_COUNT", "3")

	bl := LoadBuildLimits()
	if bl.MaxCPU == nil || bl.MaxCPU.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("MaxCPU = %v, want 4", bl.MaxCPU)
	}
	if bl.MaxMemory == nil || bl.MaxMemory.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("MaxMemory = %v, want 8Gi", bl.MaxMemory)
	}
	if bl.MaxDiskSize == nil || bl.MaxDiskSize.Cmp(resource.MustParse("100Gi")) != 0 {
		t.Errorf("MaxDiskSize = %v, want 100Gi", bl.MaxDiskSize)
	}
	if bl.MaxDiskCount != 2 {
		t.Errorf("MaxDiskCount = %d, want 2", bl.MaxDiskCount)
	}
	if bl.MaxVMCount != 3 {
		t.Errorf("MaxVMCount = %d, want 3", bl.MaxVMCount)
	}
}
