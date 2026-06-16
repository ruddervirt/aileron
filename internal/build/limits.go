package build

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// BuildLimits holds operator-wide resource limits enforced on every
// VirtualMachineBuild during the initialization reconcile. Zero/nil
// values mean "no limit".
type BuildLimits struct {
	MaxCPU       int32              // max CPU cores per VM; 0 = unlimited
	MaxMemory    *resource.Quantity // max memory per VM; nil = unlimited
	MaxDiskSize  *resource.Quantity // max size per disk; nil = unlimited
	MaxDiskCount int                // max disks per VM; 0 = unlimited
	MaxVMCount   int                // max VMs per build; 0 = unlimited
}

// LoadBuildLimits reads build limits from environment variables set by the
// Helm chart (BUILD_LIMIT_*). Missing or zero values mean "no limit".
func LoadBuildLimits() *BuildLimits {
	limits := &BuildLimits{}

	if v := os.Getenv("BUILD_LIMIT_MAX_CPU"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			limits.MaxCPU = int32(n)
		}
	}
	if v := os.Getenv("BUILD_LIMIT_MAX_MEMORY"); v != "" {
		q, err := resource.ParseQuantity(v)
		if err == nil {
			limits.MaxMemory = &q
		}
	}
	if v := os.Getenv("BUILD_LIMIT_MAX_DISK_SIZE"); v != "" {
		q, err := resource.ParseQuantity(v)
		if err == nil {
			limits.MaxDiskSize = &q
		}
	}
	if v := os.Getenv("BUILD_LIMIT_MAX_DISK_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limits.MaxDiskCount = n
		}
	}
	if v := os.Getenv("BUILD_LIMIT_MAX_VM_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limits.MaxVMCount = n
		}
	}

	return limits
}

// EnforceLimits applies build limits to a VirtualMachineBuild spec.
//
// CPU, memory, and disk size are silently clamped to the configured maximum.
// Disk count and VM count violations return an error — the caller should
// fail the build immediately.
//
// Returns (specChanged, messages, error). messages contains one human-readable
// entry per clamped field, suitable for logging or storing in the build status.
func (bl *BuildLimits) EnforceLimits(spec *v1alpha1.VirtualMachineBuildSpec) (bool, []string, error) {
	if bl == nil {
		return false, nil, nil
	}

	logger := logf.Log.WithName("build-limits")
	changed := false
	var messages []string

	// Hard fail: VM count.
	if bl.MaxVMCount > 0 && len(spec.VMs) > bl.MaxVMCount {
		return false, nil, fmt.Errorf(
			"build has %d VMs, exceeds maximum of %d",
			len(spec.VMs), bl.MaxVMCount)
	}

	for i := range spec.VMs {
		vm := &spec.VMs[i]

		// Hard fail: disk count.
		if bl.MaxDiskCount > 0 && len(vm.Disks) > bl.MaxDiskCount {
			return false, nil, fmt.Errorf(
				"VM %s has %d disks, exceeds maximum of %d",
				vm.Name, len(vm.Disks), bl.MaxDiskCount)
		}

		// Clamp CPU.
		if bl.MaxCPU > 0 && vm.Resources.CPU > bl.MaxCPU {
			msg := fmt.Sprintf("VM %s: CPU clamped from %d to %d",
				vm.Name, vm.Resources.CPU, bl.MaxCPU)
			logger.Info("Clamping CPU", "vm", vm.Name,
				"requested", vm.Resources.CPU, "max", bl.MaxCPU)
			messages = append(messages, msg)
			vm.Resources.CPU = bl.MaxCPU
			changed = true
		}

		// Clamp memory.
		if bl.MaxMemory != nil && !vm.Resources.Memory.IsZero() &&
			vm.Resources.Memory.Cmp(*bl.MaxMemory) > 0 {
			msg := fmt.Sprintf("VM %s: memory clamped from %s to %s",
				vm.Name, vm.Resources.Memory.String(), bl.MaxMemory.String())
			logger.Info("Clamping memory", "vm", vm.Name,
				"requested", vm.Resources.Memory.String(),
				"max", bl.MaxMemory.String())
			messages = append(messages, msg)
			vm.Resources.Memory = *bl.MaxMemory
			changed = true
		}

		// Clamp disk sizes.
		for j := range vm.Disks {
			disk := &vm.Disks[j]
			if bl.MaxDiskSize != nil && disk.Size.Cmp(*bl.MaxDiskSize) > 0 {
				msg := fmt.Sprintf("VM %s disk %s: size clamped from %s to %s",
					vm.Name, disk.Name, disk.Size.String(), bl.MaxDiskSize.String())
				logger.Info("Clamping disk size", "vm", vm.Name,
					"disk", disk.Name, "requested", disk.Size.String(),
					"max", bl.MaxDiskSize.String())
				messages = append(messages, msg)
				disk.Size = *bl.MaxDiskSize
				changed = true
			}
		}
	}

	return changed, messages, nil
}

// String returns a human-readable summary of the configured limits.
func (bl *BuildLimits) String() string {
	if bl == nil {
		return "<nil>"
	}
	var parts []string
	if bl.MaxCPU > 0 {
		parts = append(parts, fmt.Sprintf("maxCPU=%d", bl.MaxCPU))
	}
	if bl.MaxMemory != nil {
		parts = append(parts, fmt.Sprintf("maxMemory=%s", bl.MaxMemory.String()))
	}
	if bl.MaxDiskSize != nil {
		parts = append(parts, fmt.Sprintf("maxDiskSize=%s", bl.MaxDiskSize.String()))
	}
	if bl.MaxDiskCount > 0 {
		parts = append(parts, fmt.Sprintf("maxDiskCount=%d", bl.MaxDiskCount))
	}
	if bl.MaxVMCount > 0 {
		parts = append(parts, fmt.Sprintf("maxVMCount=%d", bl.MaxVMCount))
	}
	if len(parts) == 0 {
		return "<no limits>"
	}
	return strings.Join(parts, ", ")
}
