package build

import (
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// BuildID returns the unique identifier for a build's resources.
func BuildID(build *v1alpha1.VirtualMachineBuild) string {
	return build.Status.BuildID
}

// BuildNS returns the Kubernetes namespace where build resources are created.
func BuildNS(build *v1alpha1.VirtualMachineBuild) string {
	if build.Status.BuildNamespace != "" {
		return build.Status.BuildNamespace
	}
	return build.Namespace
}

// LabelBuildID is the label key used to associate resources with a build.
const LabelBuildID = "ruddervirt.io/build-id"

// LabelBuild is the label key for the build name.
const LabelBuild = "ruddervirt.io/build"

// LabelBuildNamespace is the label key for the build's namespace.
const LabelBuildNamespace = "ruddervirt.io/build-namespace"

// LabelVM is the label key for the VM name within a build.
const LabelVM = "ruddervirt.io/vm"

// LabelComponent is the label key for the resource's component type.
const LabelComponent = "ruddervirt.io/component"

const (
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
	PhaseRunning   = "Running"
)

// LabelOS is stamped on every VirtualMachine (build, template, clone) so the
// grader can pick the serial-console protocol without the caller having to
// specify it. Values: "windows" or "linux".
const LabelOS = "ruddervirt.io/os"

// OSForShell maps a BuildVM's shell type to the LabelOS value.
func OSForShell(shell v1alpha1.ShellType) string {
	if shell == v1alpha1.ShellTypePowerShell {
		return "windows"
	}
	return "linux"
}

// DefaultDisks returns the disks for a VM, providing a single 20Gi virtio
// disk if none are specified.
func DefaultDisks(vmSpec *v1alpha1.BuildVM) []v1alpha1.BuildDisk {
	if len(vmSpec.Disks) > 0 {
		return vmSpec.Disks
	}
	return []v1alpha1.BuildDisk{{
		Name: "rootdisk",
		Size: resource.MustParse("20Gi"),
		Bus:  "virtio",
	}}
}

// BootDisk returns the first (boot) disk for a VM.
func BootDisk(vmSpec *v1alpha1.BuildVM) v1alpha1.BuildDisk {
	return DefaultDisks(vmSpec)[0]
}

// DiskDVName returns the DataVolume name for a disk.
// The boot disk (index 0) uses the existing naming convention.
// Additional disks append the disk name.
func DiskDVName(buildID, vmName string, diskIndex int, diskName string) string {
	if diskIndex == 0 {
		return BuildNameForBuildVMDataVolume(buildID, vmName)
	}
	return buildID + "-" + vmName + "-" + diskName
}
