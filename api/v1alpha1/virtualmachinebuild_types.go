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

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Phase enums ---

// BuildPhase represents the overall build phase.
// +kubebuilder:validation:Enum=Pending;Networking;Building;CapturingDisks;Exporting;TemplateProvisioning;Succeeded;Failed
type BuildPhase string

const (
	BuildPhasePending              BuildPhase = "Pending"
	BuildPhaseNetworking           BuildPhase = "Networking"
	BuildPhaseBuilding             BuildPhase = "Building"
	BuildPhaseCapturingDisks       BuildPhase = "CapturingDisks"
	BuildPhaseExporting            BuildPhase = "Exporting"
	BuildPhaseTemplateProvisioning BuildPhase = "TemplateProvisioning"
	BuildPhaseSucceeded            BuildPhase = "Succeeded"
	BuildPhaseFailed               BuildPhase = "Failed"
)

// VMPhase represents the phase of an individual VM within a build.
// +kubebuilder:validation:Enum=Pending;SourceImporting;Booting;BootCommand;Provisioning;ShuttingDown;DiskCaptured;Succeeded;Failed
type VMPhase string

const (
	VMPhasePending         VMPhase = "Pending"
	VMPhaseSourceImporting VMPhase = "SourceImporting"
	VMPhaseBooting         VMPhase = "Booting"
	VMPhaseBootCommand     VMPhase = "BootCommand"
	VMPhaseProvisioning    VMPhase = "Provisioning"
	VMPhaseShuttingDown    VMPhase = "ShuttingDown"
	VMPhaseDiskCaptured    VMPhase = "DiskCaptured"
	VMPhaseSucceeded       VMPhase = "Succeeded"
	VMPhaseFailed          VMPhase = "Failed"
)

// ProvisionerType represents the type of provisioner.
// +kubebuilder:validation:Enum=shell;file;reboot;windows-update;handbuild
type ProvisionerType string

const (
	ProvisionerTypeShell         ProvisionerType = "shell"
	ProvisionerTypeFile          ProvisionerType = "file"
	ProvisionerTypeReboot        ProvisionerType = "reboot"
	ProvisionerTypeWindowsUpdate ProvisionerType = "windows-update"
	ProvisionerTypeHandbuild     ProvisionerType = "handbuild"
)

// CommunicatorType represents the method used to communicate with a build VM.
// +kubebuilder:validation:Enum=ssh
type CommunicatorType string

const (
	CommunicatorTypeSSH CommunicatorType = "ssh"
)

// ShellType represents the shell environment of a build VM.
// Determines how files are uploaded and scripts are executed over SSH.
// +kubebuilder:validation:Enum=bash;powershell
type ShellType string

const (
	ShellTypeBash       ShellType = "bash"
	ShellTypePowerShell ShellType = "powershell"
)

// DiskFormat represents the disk image format for export.
// +kubebuilder:validation:Enum=raw;qcow2
type DiskFormat string

const (
	DiskFormatRaw   DiskFormat = "raw"
	DiskFormatQcow2 DiskFormat = "qcow2"
)

// =============================================================================
// Per-VM Spec
// =============================================================================

// BuildVM defines a single VM within a multi-VM build.
type BuildVM struct {
	// name is the unique identifier for this VM within the build.
	// Used for DNS resolution between VMs (e.g., other VMs can reach this one by name).
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// source defines the base disk image for this VM.
	// +required
	Source BuildSource `json:"source"`

	// resources defines compute resources for this VM. Required: cpu and
	// memory must be set explicitly.
	// +required
	Resources BuildResources `json:"resources"`

	// disks defines the disks attached to this VM.
	// For url/sourcePvc/containerDisk/blank sources, the first disk is the boot
	// disk and receives the source image; additional disks are created blank.
	// For a buildRef source the boot disk is inherited from the parent build, so
	// every disk listed here is treated as an additional blank data disk.
	// If empty, a single 20Gi virtio disk is created.
	// +optional
	Disks []BuildDisk `json:"disks,omitempty"`

	// communicator configures how to connect to this VM for provisioning.
	// +optional
	Communicator BuildCommunicator `json:"communicator,omitempty"`

	// cloudInit enables cloud-init and defines its configuration for this VM.
	// When set to true (or with configuration), a cloud-init disk is attached.
	// +optional
	CloudInit *BuildCloudInit `json:"cloudInit,omitempty"`

	// nics defines the network interfaces for this VM.
	// +optional
	NICs []VMNIC `json:"nics,omitempty"`

	// bootCommand sends keystrokes to the VM via VNC console before provisioning.
	// Uses Packer-compatible syntax: <enter>, <tab>, <wait>, <wait5>, etc.
	// +optional
	BootCommand []string `json:"bootCommand,omitempty"`

	// efiFirmware enables UEFI boot with custom OVMF firmware.
	// +optional
	EFIFirmware *EFIFirmware `json:"efiFirmware,omitempty"`

	// provisioners is an ordered list of provisioning steps for this VM.
	// +optional
	Provisioners []BuildProvisioner `json:"provisioners,omitempty"`

	// isos defines ISO images to attach as cdrom devices.
	// ISOs are cached as ReadOnlyMany PVCs and shared across builds.
	// +optional
	ISOs []ISOSource `json:"isos,omitempty"`

	// floppy attaches a floppy disk containing files from spec.files.
	// +optional
	Floppy *Floppy `json:"floppy,omitempty"`
}

// EFIFirmware configures UEFI boot with custom OVMF firmware files.
type EFIFirmware struct {
	// secureBoot enables UEFI Secure Boot. Defaults to false.
	// +optional
	SecureBoot bool `json:"secureBoot"`
}

// =============================================================================
// Source
// =============================================================================

// BuildSource defines where the base image comes from.
// Exactly one of url, sourcePvc, containerDisk, or buildRef must be set.
type BuildSource struct {
	// url is an HTTP/HTTPS URL to a disk image. CDI will import it.
	// +optional
	URL string `json:"url,omitempty"`

	// sourcePVC references an existing PVC containing a base disk image.
	// +optional
	SourcePVC *PVCReference `json:"sourcePvc,omitempty"`

	// containerDisk is a container image reference containing a disk image.
	// +optional
	ContainerDisk string `json:"containerDisk,omitempty"`

	// buildRef references the output of a previous VirtualMachineBuild.
	// The referenced build must be in Succeeded phase; the build fails immediately if it is not.
	// +optional
	BuildRef *BuildReference `json:"buildRef,omitempty"`

	// blank creates an empty disk. Use this with isos for OS installation from ISO media.
	// +optional
	Blank bool `json:"blank,omitempty"`
}

// PVCReference is a reference to a PVC by name and namespace.
type PVCReference struct {
	// name of the PVC.
	// +required
	Name string `json:"name"`

	// namespace of the PVC.
	// +required
	Namespace string `json:"namespace"`
}

// BuildReference references the output of a previous VirtualMachineBuild.
type BuildReference struct {
	// name is the name of the VirtualMachineBuild to source from.
	// +required
	Name string `json:"name"`

	// namespace of the VirtualMachineBuild. Defaults to the current build's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// vmName is the name of the VM within the referenced build whose output disk to use.
	// Required when the referenced build has multiple VMs.
	// +optional
	VMName string `json:"vmName,omitempty"`
}

// =============================================================================
// ISO
// =============================================================================

// ISOSource defines an ISO image to attach as a cdrom device.
// ISOs are imported once and cached as ReadOnlyMany PVCs keyed by checksum.
type ISOSource struct {
	// url is an HTTP/HTTPS URL to an ISO image.
	// +required
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// checksum is an optional SHA-256 hex digest of the ISO for cache keying and verification.
	// If omitted, the SHA-256 hash of the URL is used as the cache key.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-fA-F0-9]{64}$`
	Checksum string `json:"checksum,omitempty"`
}

// =============================================================================
// Resources
// =============================================================================

// BuildResources defines compute resources for a build VM.
type BuildResources struct {
	// cpu is the amount of CPU for the VM. Required. May be fractional (e.g.
	// "0.1" or "100m") to pack idle VMs densely; quote fractional values. The
	// guest's vCPU core count is derived as ceil(cpu) with a minimum of 1, and
	// this same value is used as the pod's CPU request
	// (spec.domain.resources.requests.cpu).
	// +required
	CPU resource.Quantity `json:"cpu"`

	// memory is the amount of RAM for the VM (e.g. "4Gi"). Required. Used as
	// both the guest memory and the pod memory request.
	// +required
	Memory resource.Quantity `json:"memory"`
}

// BuildDisk defines a disk attached to a build VM.
// The first disk is the boot disk and receives the source image.
// Additional disks are created blank.
type BuildDisk struct {
	// name is the disk identifier within the VM.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// size is the disk capacity (e.g., "25Gi").
	// +required
	Size resource.Quantity `json:"size"`

	// bus is the disk bus type. Defaults to "virtio".
	// "usb" presents the disk to the guest as a removable (non-fixed) drive,
	// which Windows reports as removable media; it is not bootable and is
	// slower than virtio, so use it only for data/transfer disks.
	// +kubebuilder:validation:Enum=virtio;scsi;sata;usb
	// +kubebuilder:default="virtio"
	// +optional
	Bus string `json:"bus,omitempty"`

	// storageClass overrides the default StorageClass for this disk.
	// +optional
	StorageClass *string `json:"storageClass,omitempty"`
}

// =============================================================================
// Communicator
// =============================================================================

// BuildCommunicator configures how the operator connects to a build VM.
type BuildCommunicator struct {
	// type is the communicator type. Currently only "ssh" is supported.
	// +kubebuilder:default=ssh
	// +optional
	Type CommunicatorType `json:"type,omitempty"`

	// shell is the guest shell type. Determines how files are uploaded and
	// scripts are executed. "bash" for POSIX systems (Linux, BSD, macOS),
	// "powershell" for Windows. Defaults to "bash".
	// +kubebuilder:default=bash
	// +optional
	Shell ShellType `json:"shell,omitempty"`

	// sshUsername is the user to SSH as. Defaults to "root".
	// +kubebuilder:default="root"
	// +optional
	SSHUsername string `json:"sshUsername,omitempty"`

	// sshPort is the SSH port. Defaults to 22.
	// +kubebuilder:default=22
	// +optional
	SSHPort int32 `json:"sshPort,omitempty"`

	// sshPassword is the password for SSH authentication.
	// When set, password auth is used instead of (or in addition to) key auth.
	// +optional
	SSHPassword string `json:"sshPassword,omitempty"`

	// sshPrivateKeySecret references a Secret containing a pre-existing SSH private key.
	// If not set, a keypair is auto-generated per build.
	// +optional
	SSHPrivateKeySecret *SecretKeyReference `json:"sshPrivateKeySecret,omitempty"`

	// sshTimeout is how long to wait for SSH to become available before failing.
	// Increase this for ISO installs that take longer to complete. Defaults to "5m".
	// +optional
	SSHTimeout *metav1.Duration `json:"sshTimeout,omitempty"`
}

// SecretKeyReference is a reference to a specific key in a Secret.
type SecretKeyReference struct {
	// name of the Secret.
	// +required
	Name string `json:"name"`

	// key within the Secret data.
	// +required
	Key string `json:"key"`
}

// =============================================================================
// Cloud-Init
// =============================================================================

// BuildCloudInit defines cloud-init configuration injected into a build VM.
// The cloud-init disk is only attached when this struct is present.
type BuildCloudInit struct {
	// userData is inline cloud-init user-data (cloud-config format).
	// +optional
	UserData string `json:"userData,omitempty"`

	// userDataFrom references a ConfigMap or Secret containing user-data.
	// +optional
	UserDataFrom *ConfigMapOrSecretReference `json:"userDataFrom,omitempty"`

	// networkData is inline cloud-init network-data.
	// +optional
	NetworkData string `json:"networkData,omitempty"`
}

// ConfigMapOrSecretReference references either a ConfigMap or Secret key.
type ConfigMapOrSecretReference struct {
	// configMapRef references a ConfigMap key.
	// +optional
	ConfigMapRef *corev1.ConfigMapKeySelector `json:"configMapRef,omitempty"`

	// secretRef references a Secret key.
	// +optional
	SecretRef *corev1.SecretKeySelector `json:"secretRef,omitempty"`
}

// =============================================================================
// Provisioners
// =============================================================================

// BuildProvisioner defines a single provisioning step.
type BuildProvisioner struct {
	// type is the provisioner type: "shell", "file", "reboot", or "windows-update".
	// +required
	Type ProvisionerType `json:"type"`

	// name is an optional human-readable name for this step.
	// +optional
	Name string `json:"name,omitempty"`

	// stepTimeout is the maximum duration for this step (e.g. "30m", "2h").
	// If not set, the step runs until the build-level timeout expires.
	// +optional
	StepTimeout string `json:"stepTimeout,omitempty"`

	// shell configures a shell provisioner. Required when type is "shell".
	// +optional
	Shell *ShellProvisioner `json:"shell,omitempty"`

	// file configures a file provisioner. Required when type is "file".
	// +optional
	File *FileProvisioner `json:"file,omitempty"`

	// reboot configures a reboot provisioner. Optional when type is "reboot".
	// +optional
	Reboot *RebootProvisioner `json:"reboot,omitempty"`

	// windowsUpdate configures a Windows Update provisioner. Optional when type is "windows-update".
	// +optional
	WindowsUpdate *WindowsUpdateProvisioner `json:"windowsUpdate,omitempty"`

	// handbuild configures a handbuild provisioner. Required when type is "handbuild".
	// +optional
	Handbuild *HandbuildProvisioner `json:"handbuild,omitempty"`
}

// ShellProvisioner runs shell commands on a build VM.
// Each shell provisioner runs a single script. Use multiple provisioner
// steps for multiple scripts — variables and state do not carry across steps.
type ShellProvisioner struct {
	// inline is the script to run.
	// +optional
	Inline string `json:"inline,omitempty"`

	// scriptFrom references a script stored in a ConfigMap or Secret.
	// +optional
	ScriptFrom *ConfigMapOrSecretReference `json:"scriptFrom,omitempty"`

	// env is a map of environment variables to set before running commands.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// executeCommand overrides the default command wrapper.
	// Use {{ .Command }} as a placeholder for the actual command.
	// +optional
	ExecuteCommand string `json:"executeCommand,omitempty"`
}

// FileProvisioner uploads files to a build VM.
// Exactly one of source or fileRef must be set.
type FileProvisioner struct {
	// source references the file content from a ConfigMap or Secret.
	// +optional
	Source *ConfigMapOrSecretReference `json:"source,omitempty"`

	// fileRef references a file in spec.files by name.
	// +optional
	FileRef string `json:"fileRef,omitempty"`

	// destination is the absolute path on the build VM to write the file.
	// +required
	Destination string `json:"destination"`
}

// RebootProvisioner reboots a build VM and waits for it to come back.
// Sends a reboot command, waits for SSH to drop (confirming reboot started),
// then waits for SSH to come back (confirming reboot completed).
type RebootProvisioner struct {
	// command overrides the default reboot command.
	// Defaults to "shutdown /r /f /t 0" for PowerShell or "sudo reboot" for bash.
	// +optional
	Command string `json:"command,omitempty"`
}

// WindowsUpdateProvisioner installs Windows Updates on a build VM.
// Implements the same search/filter/reboot loop as packer-plugin-windows-update.
// Automatically reboots as needed and re-searches until no more updates match.
type WindowsUpdateProvisioner struct {
	// searchCriteria is the Windows Update Agent search string.
	// Defaults to "BrowseOnly=0 and IsInstalled=0" (recommended updates).
	// Common values:
	//   "AutoSelectOnWebSites=1 and IsInstalled=0"  (important only)
	//   "BrowseOnly=0 and IsInstalled=0"            (recommended, default)
	//   "IsInstalled=0"                              (all available)
	// +optional
	SearchCriteria string `json:"searchCriteria,omitempty"`

	// filters is a list of PowerShell filter expressions applied to each update.
	// Format: "include:EXPRESSION" or "exclude:EXPRESSION".
	// $_ is the IUpdate object. Evaluated in order; first match wins.
	// Example: ["exclude:$_.Title -like '*Preview*'", "include:$true"]
	// +optional
	Filters []string `json:"filters,omitempty"`

	// updateLimit is the maximum number of updates to install per cycle
	// before rebooting. Defaults to 1000.
	// +optional
	UpdateLimit int `json:"updateLimit,omitempty"`
}

// HandbuildProvisioner pauses the build so a human can interact with the VM
// via VNC. The build resumes when a "continue" signal is sent via NATS.
type HandbuildProvisioner struct {
	// instructions describes what the human operator should do via VNC.
	// Displayed in the UI while the build is paused.
	// +required
	Instructions string `json:"instructions"`
}

// =============================================================================
// Output
// =============================================================================

// BuildOutput defines what artifacts are produced after a successful build.
type BuildOutput struct {
	// vms maps each build VM to its output artifact.
	// +required
	// +kubebuilder:validation:MinItems=1
	VMs []VMOutput `json:"vms"`
}

// VMOutput defines the output artifact for a single VM.
type VMOutput struct {
	// name must match a VM name in spec.vms[].
	// +required
	Name string `json:"name"`

	// dataVolume configures the output DataVolume/PVC for this VM.
	// +required
	DataVolume DataVolumeOutput `json:"dataVolume"`

	// s3Export optionally exports this VM's disk image to S3.
	// +optional
	S3Export *S3ExportOutput `json:"s3Export,omitempty"`
}

// DataVolumeOutput configures an output DataVolume.
type DataVolumeOutput struct {
	// name of the output DataVolume.
	// +required
	Name string `json:"name"`

	// namespace of the output DataVolume. Defaults to the build's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// storageClass for the output DataVolume.
	// +optional
	StorageClass *string `json:"storageClass,omitempty"`

	// accessModes for the output PVC.
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// S3ExportOutput configures exporting a built image to S3-compatible storage.
type S3ExportOutput struct {
	// endpoint is the S3 endpoint URL.
	// +required
	Endpoint string `json:"endpoint"`

	// bucket is the S3 bucket name.
	// +required
	Bucket string `json:"bucket"`

	// key is the S3 object key (path) for the uploaded image.
	// +required
	Key string `json:"key"`

	// format is the disk image format to export. Defaults to "raw".
	// +kubebuilder:default=raw
	// +optional
	Format DiskFormat `json:"format,omitempty"`

	// region is the S3 region. Optional for non-AWS S3-compatible stores.
	// +optional
	Region string `json:"region,omitempty"`

	// credentialsSecret references a Secret with keys AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
	// +required
	CredentialsSecret corev1.LocalObjectReference `json:"credentialsSecret"`
}

// =============================================================================
// Files
// =============================================================================

// BuildFile defines a named file that can be referenced by httpDirectory,
// floppy, or file provisioners. Exactly one of inline or url must be set.
type BuildFile struct {
	// name is the unique identifier for this file within the build and is
	// ALSO the filename used downstream (the path on the floppy disk and
	// the URL path served by httpDirectory). Use a fully-qualified
	// filename with extension (e.g. "Autounattend.xml", "netkvm.inf",
	// "preseed.cfg"). Referenced by httpDirectory.files, floppy.files,
	// and file provisioner fileRef.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// inline provides the file content directly as a string.
	// +optional
	Inline string `json:"inline,omitempty"`

	// url is an HTTP/HTTPS URL to download the file from at build time.
	// +optional
	URL string `json:"url,omitempty"`
}

// =============================================================================
// HTTP Directory
// =============================================================================

// HttpDirectory configures an HTTP file server for boot-time file serving.
// Files are sourced from spec.files.
type HttpDirectory struct {
	// files references files from spec.files to serve via HTTP.
	// +required
	// +kubebuilder:validation:MinItems=1
	Files []HttpDirectoryFileRef `json:"files"`
}

// HttpDirectoryFileRef references a file to serve via HTTP.
// The referenced file's name (from spec.files) is also the URL path it's
// served at, so file names should be fully-qualified filenames
// (e.g., "preseed.cfg").
type HttpDirectoryFileRef struct {
	// name references a file in spec.files by name. The name is used
	// as-is for the URL path.
	// +required
	Name string `json:"name"`
}

// =============================================================================
// Floppy
// =============================================================================

// Floppy attaches a floppy disk containing the referenced files to a build VM.
// Useful for Windows unattend/sysprep or BIOS-based boot configurations.
type Floppy struct {
	// files references files from spec.files to include on the floppy disk.
	// +required
	// +kubebuilder:validation:MinItems=1
	Files []FloppyFileRef `json:"files"`
}

// FloppyFileRef references a file to include on the floppy disk.
// The referenced file's name (from spec.files) is also the filename written
// to the floppy, so file names should be fully-qualified filenames
// (e.g., "Autounattend.xml", "netkvm.inf").
type FloppyFileRef struct {
	// name references a file in spec.files by name. The name is used
	// as-is for the filename on the floppy disk.
	// +required
	Name string `json:"name"`
}

// =============================================================================
// Build Overrides
// =============================================================================

// BuildOverrides defines settings that apply only during the build phase.
// These values do NOT carry forward to the template that clones inherit —
// the base spec values are what gets captured. Overrides allow the build
// phase to temporarily deviate from the clone baseline (e.g., builds need
// internet to install packages, clones don't; a build needs an extra NIC on
// a provisioning subnet that clones shouldn't see; or a segment that is
// unmanaged for clones must be managed during the build so provisioning has
// DHCP/relay reachability before the guest gateway exists).
type BuildOverrides struct {
	// vpcs overrides per-VPC settings during the build phase only.
	// Each entry must match a VPC name in spec.network.vpcs[].
	// +optional
	VPCs []VPCOverride `json:"vpcs,omitempty"`

	// subnets overrides per-subnet settings during the build phase only.
	// Each entry must match a subnet name in spec.network.subnets[].
	// +optional
	Subnets []SubnetOverride `json:"subnets,omitempty"`

	// vms overrides per-VM settings during the build phase only.
	// Each entry must match a VM name in spec.vms[].
	// +optional
	VMs []BuildVMOverride `json:"vms,omitempty"`
}

// VPCOverride overrides VPC settings during the build phase only.
type VPCOverride struct {
	// name must match a VPC name in spec.network.vpcs[].
	// +required
	Name string `json:"name"`

	// internet overrides the VPC's internet setting during the build phase.
	// The base spec.network.vpcs[].internet value is what clones inherit.
	// +optional
	Internet *bool `json:"internet,omitempty"`
}

// SubnetOverride overrides subnet settings during the build phase only.
type SubnetOverride struct {
	// name must match a subnet name in spec.network.subnets[].
	// +required
	Name string `json:"name"`

	// unmanaged overrides the subnet's unmanaged setting during the build
	// phase. The base spec.network.subnets[].unmanaged value is what clones
	// inherit. Set this to false to realize a segment as MANAGED during the
	// build (OVN DHCP + relay reachability) while keeping it unmanaged for
	// clones: a guest-served (unmanaged) segment has no DHCP during its own
	// build because the guest gateway that will serve it doesn't exist yet,
	// so build-time provisioning (config fetch, SSH file provisioners) has
	// nothing to lease from. Overriding it managed lets provisioning ride
	// the normal OVN path; the guest then owns the segment once cloned.
	// +optional
	Unmanaged *bool `json:"unmanaged,omitempty"`
}

// BuildVMOverride overrides per-VM settings during the build phase only.
type BuildVMOverride struct {
	// name must match a VM name in spec.vms[].
	// +required
	Name string `json:"name"`

	// resources overrides the VM's compute resources during the build phase.
	// The base spec.vms[].resources value is what clones inherit.
	// +optional
	Resources *BuildResources `json:"resources,omitempty"`

	// nics overrides the VM's NIC list during the build phase only.
	// When non-empty, fully replaces spec.vms[].nics for the build VM —
	// the base list is still what gets captured into the template and
	// inherited by clones. Use this to attach build-only NICs (e.g., a
	// provisioning subnet with internet) without leaking them downstream.
	// +optional
	NICs []VMNIC `json:"nics,omitempty"`
}

// =============================================================================
// Top-level Spec
// =============================================================================

// VirtualMachineBuildSpec defines the desired state of VirtualMachineBuild.
type VirtualMachineBuildSpec struct {
	// vms defines the VMs to build. All VMs boot and provision in parallel.
	// VMs can communicate with each other by name over the defined network.
	// +required
	// +kubebuilder:validation:MinItems=1
	VMs []BuildVM `json:"vms"`

	// files defines named files that can be referenced by httpDirectory, floppy,
	// or file provisioners. Supports inline content and URL sources.
	// +optional
	Files []BuildFile `json:"files,omitempty"`

	// network defines the KubeOVN network topology (VPCs, subnets) for the build.
	// Per-VM NIC assignments are defined on each VM in spec.vms[].nics[].
	// +optional
	Network *Network `json:"network,omitempty"`

	// output is DEPRECATED and ignored. All VMs are automatically captured.
	// +optional
	Output *BuildOutput `json:"output,omitempty"`

	// s3Export optionally exports all built VM disk images to S3.
	// +optional
	S3Export *S3ExportOutput `json:"s3Export,omitempty"`

	// httpDirectory references a ConfigMap whose data keys are served via HTTP
	// to build VMs during boot. Use {{ .HTTPIP }} and {{ .HTTPPort }} in
	// bootCommand strings to reference the server.
	// +optional
	HttpDirectory *HttpDirectory `json:"httpDirectory,omitempty"`

	// namespace overrides the auto-generated child namespace name.
	// When set, the controller uses this namespace and skips deletion on cleanup.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// namespacePrefix is the prefix for the auto-generated child namespace.
	// The generated name is {prefix}{uid-hash} (e.g. "vm-a3f8b2c1").
	// Defaults to "vm-".
	// +kubebuilder:default="vm-"
	// +optional
	NamespacePrefix string `json:"namespacePrefix,omitempty"`

	// buildOverrides applies settings that take effect ONLY during the build phase.
	// The base spec fields (spec.network.vpcs, spec.vms[].resources) are what
	// gets captured in the template for clones. Overrides let the build phase
	// differ from the clone phase — for example, a build can have internet access
	// while clones don't, or a build can use extra CPU for compilation.
	// +optional
	BuildOverrides *BuildOverrides `json:"buildOverrides,omitempty"`

	// timeout is the maximum duration for the entire build. Defaults to "30m".
	// +kubebuilder:default="30m"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// retries is the number of times to retry the build on failure.
	// +kubebuilder:default=0
	// +optional
	Retries int32 `json:"retries,omitempty"`

	// isoCacheTTL is how long imported ISO PVCs are kept before cleanup.
	// Defaults to 24h.
	// +optional
	ISOCacheTTL *metav1.Duration `json:"isoCacheTTL,omitempty"`
}

// =============================================================================
// Status
// =============================================================================

// ProvisionerResult records the outcome of a single provisioner step.
type ProvisionerResult struct {
	// index is the position in the provisioners list.
	Index int32 `json:"index"`

	// type is the provisioner type.
	Type ProvisionerType `json:"type"`

	// name is the provisioner name (if set).
	// +optional
	Name string `json:"name,omitempty"`

	// status is the result: Pending, Running, Succeeded, or Failed.
	// +optional
	Status string `json:"status,omitempty"`

	// duration is how long this step took.
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`

	// message contains error details on failure.
	// +optional
	Message string `json:"message,omitempty"`
}

// VMBuildStatus tracks the state of a single VM within the build.
type VMBuildStatus struct {
	// name is the VM name (matches spec.vms[].name).
	Name string `json:"name"`

	// phase is the current phase of this VM's build.
	// +optional
	Phase VMPhase `json:"phase,omitempty"`

	// vmName is the name of the ephemeral KubeVirt VM.
	// +optional
	VMName string `json:"vmName,omitempty"`

	// message is a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// outputDataVolume is the namespace/name of the output DataVolume.
	// +optional
	OutputDataVolume string `json:"outputDataVolume,omitempty"`

	// provisionerResults records the outcome of each provisioner step for this VM.
	// +optional
	ProvisionerResults []ProvisionerResult `json:"provisionerResults,omitempty"`
}

// S3ExportStatus records the state of an S3 export.
type S3ExportStatus struct {
	// vmName is the VM this export belongs to.
	VMName string `json:"vmName"`

	// uploaded indicates whether the upload completed.
	Uploaded bool `json:"uploaded"`

	// location is the full S3 URI.
	// +optional
	Location string `json:"location,omitempty"`

	// checksum is the SHA-256 checksum of the uploaded image.
	// +optional
	Checksum string `json:"checksum,omitempty"`
}

// NetworkStatus records the state of the build's network resources.
type NetworkStatus struct {
	// vpcsCreated lists the KubeOVN VPC names created for this build.
	// +optional
	VPCsCreated []string `json:"vpcsCreated,omitempty"`

	// subnetsCreated lists the KubeOVN Subnet names created for this build.
	// +optional
	SubnetsCreated []string `json:"subnetsCreated,omitempty"`

	// ready indicates all network resources are available.
	Ready bool `json:"ready"`
}

// VirtualMachineBuildStatus defines the observed state of VirtualMachineBuild.
type VirtualMachineBuildStatus struct {
	// phase is the overall build phase.
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`

	// buildID is the unique identifier for this build's resources.
	// All resources created by this build are prefixed with this ID and labeled
	// with ruddervirt.io/build-id for cleanup. Generated once on first reconcile.
	// +optional
	BuildID string `json:"buildID,omitempty"`

	// buildNamespace is the Kubernetes namespace where build resources are created.
	// Defaults to the operator namespace (ruddervirt-system).
	// +optional
	BuildNamespace string `json:"buildNamespace,omitempty"`

	// virtualMachineNamespace is the name of the VirtualMachineNamespace CR
	// that logically groups this build's resources.
	// +optional
	VirtualMachineNamespace string `json:"virtualMachineNamespace,omitempty"`

	// templateNamespace is the namespace containing output PVCs and halted template VMs.
	// After a successful build this equals buildNamespace (the build namespace becomes the template namespace).
	// +optional
	TemplateNamespace string `json:"templateNamespace,omitempty"`

	// startTime is when the build started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime is when the build finished.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// network records the state of network resources.
	// +optional
	Network *NetworkStatus `json:"network,omitempty"`

	// vmStatuses tracks the state of each VM in the build.
	// +optional
	VMStatuses []VMBuildStatus `json:"vmStatuses,omitempty"`

	// s3Exports records the state of S3 exports.
	// +optional
	S3Exports []S3ExportStatus `json:"s3Exports,omitempty"`

	// retryCount is the number of retries attempted.
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`

	// message is a human-readable summary of the build state.
	// +optional
	Message string `json:"message,omitempty"`

	// conditions represent the latest available observations.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types for VirtualMachineBuild.
const (
	ConditionNetworkReady        = "NetworkReady"
	ConditionAllVMsReady         = "AllVMsReady"
	ConditionDisksCapture        = "DisksCaptured"
	ConditionExported            = "Exported"
	ConditionTemplateProvisioned = "TemplateProvisioned"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vmb
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Build-ID",type=string,JSONPath=`.status.buildID`
// +kubebuilder:printcolumn:name="VMs",type=integer,JSONPath=`.status.vmStatuses[*].name`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualMachineBuild is the Schema for the virtualmachinebuilds API.
// It defines a multi-VM image build pipeline with KubeOVN networking.
type VirtualMachineBuild struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VirtualMachineBuild.
	// +required
	Spec VirtualMachineBuildSpec `json:"spec"`

	// status defines the observed state of VirtualMachineBuild.
	// +optional
	Status VirtualMachineBuildStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VirtualMachineBuildList contains a list of VirtualMachineBuild.
type VirtualMachineBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VirtualMachineBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualMachineBuild{}, &VirtualMachineBuildList{})
}
