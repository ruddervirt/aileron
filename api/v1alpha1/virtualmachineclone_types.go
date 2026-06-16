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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Phase enums ---

// ClonePhase represents the overall clone phase.
// +kubebuilder:validation:Enum=Pending;Validating;SnapshotSelection;VolumeProvisioning;Networking;VMProvisioning;Ready;Failed
type ClonePhase string

const (
	ClonePhasePending            ClonePhase = "Pending"
	ClonePhaseValidating         ClonePhase = "Validating"
	ClonePhaseSnapshotSelection  ClonePhase = "SnapshotSelection"
	ClonePhaseVolumeProvisioning ClonePhase = "VolumeProvisioning"
	ClonePhaseNetworking         ClonePhase = "Networking"
	ClonePhaseVMProvisioning     ClonePhase = "VMProvisioning"
	ClonePhaseReady              ClonePhase = "Ready"
	ClonePhaseFailed             ClonePhase = "Failed"
)

// EgressGatewaySpec controls the VpcEgressGateway for a clone.
type EgressGatewaySpec struct {
	// enabled controls whether the egress gateway pod runs.
	// When false, the gateway is scaled to 0, cutting internet access.
	// Defaults to true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// CloneVolumePhase represents the progress of an individual volume during cloning.
// +kubebuilder:validation:Enum=Pending;SnapshotSelected;SnapshotReady;PersistentVolumeReady;PVCBound;Complete
type CloneVolumePhase string

const (
	CloneVolumePhasePending               CloneVolumePhase = "Pending"
	CloneVolumePhaseSnapshotSelected      CloneVolumePhase = "SnapshotSelected"
	CloneVolumePhaseSnapshotReady         CloneVolumePhase = "SnapshotReady"
	CloneVolumePhasePersistentVolumeReady CloneVolumePhase = "PersistentVolumeReady"
	CloneVolumePhasePVCBound              CloneVolumePhase = "PVCBound"
	CloneVolumePhaseComplete              CloneVolumePhase = "Complete"
)

// =============================================================================
// Per-VM overrides
// =============================================================================

// VMOverride allows customization of a cloned VM.
type VMOverride struct {
	// name must match a VM name in the template namespace.
	// +required
	Name string `json:"name"`

	// nics overrides the NIC assignments for this VM.
	// +optional
	NICs []VMNIC `json:"nics,omitempty"`

	// resources overrides compute/storage for this VM.
	// +optional
	Resources *BuildResources `json:"resources,omitempty"`
}

// =============================================================================
// Top-level Spec
// =============================================================================

// VirtualMachineCloneSpec defines the desired state of VirtualMachineClone.
type VirtualMachineCloneSpec struct {
	// templateName is the golden template name. The template namespace is resolved
	// as vm-{templateName} and must contain pre-configured VMs with their disks.
	// +required
	TemplateName string `json:"templateName"`

	// namespace overrides the auto-generated child namespace name.
	// When set, the controller uses this namespace and skips deletion on cleanup.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// namespacePrefix is the prefix for the auto-generated child namespace.
	// The generated name is {prefix}{uid-hash} (e.g. "ns-38ca284f").
	// Defaults to "ns-".
	// +kubebuilder:default="ns-"
	// +optional
	NamespacePrefix string `json:"namespacePrefix,omitempty"`

	// network defines the KubeOVN network topology for the cloned VMs.
	// If omitted, networking configuration is derived from the template VMs.
	// +optional
	Network *Network `json:"network,omitempty"`

	// vmOverrides allows per-VM customization of the cloned VMs.
	// +optional
	VMOverrides []VMOverride `json:"vmOverrides,omitempty"`

	// timeout is the maximum duration for the entire clone operation. Defaults to "15m".
	// +kubebuilder:default="15m"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// egressGateway controls the VpcEgressGateway for this clone.
	// Set enabled=false to scale the egress gateway to 0 (cuts internet).
	// VM power is managed directly via KubeVirt (virtctl stop/start).
	// +optional
	EgressGateway *EgressGatewaySpec `json:"egressGateway,omitempty"`

	// replacesCloneID names a prior VirtualMachineClone (by its status.cloneID)
	// whose age this clone should inherit. The controller looks up the
	// predecessor and uses its status.ageAnchor (if set) or its
	// metadata.creationTimestamp as the age anchor for this clone's VMs.
	// When set, the watchdog deletes this clone's VMs at the same time the
	// predecessor's would have been deleted.
	// +optional
	ReplacesCloneID string `json:"replacesCloneID,omitempty"`

	// ageAnchor is an explicit RFC3339 timestamp used as the age anchor when
	// the predecessor named in replacesCloneID cannot be found, or when the
	// caller wants to set the anchor directly. Lookup by replacesCloneID
	// takes priority when the predecessor exists.
	// +optional
	AgeAnchor *metav1.Time `json:"ageAnchor,omitempty"`
}

// =============================================================================
// Status
// =============================================================================

// CloneVolumeStatus captures the state of a single volume during cloning.
type CloneVolumeStatus struct {
	// volumeName corresponds to the VirtualMachine volume definition name.
	VolumeName string `json:"volumeName"`

	// sourceVMName records the full name of the source VirtualMachine owning
	// this volume (i.e., the template VM's metadata.name).
	// +optional
	SourceVMName string `json:"sourceVmName,omitempty"`

	// sourceVMShortName records the short VM name (spec.vms[].name) copied
	// from the template VM's ruddervirt.io/vm label. Used to derive clone-side
	// resource names without having to parse the template VM's full name.
	// +optional
	SourceVMShortName string `json:"sourceVmShortName,omitempty"`

	// sourcePVCName records the source PVC backing the template volume.
	// +optional
	SourcePVCName string `json:"sourcePvcName,omitempty"`

	// sourcePVName stores the PersistentVolume bound to the source PVC.
	// +optional
	SourcePVName string `json:"sourcePvName,omitempty"`

	// csiDriver records the CSI driver managing the source volume.
	// +optional
	CSIDriver string `json:"csiDriver,omitempty"`

	// snapshotName is the VolumeSnapshot selected for restore.
	// +optional
	SnapshotName string `json:"snapshotName,omitempty"`

	// snapshotContentName is the VolumeSnapshotContent backing the selected snapshot.
	// +optional
	SnapshotContentName string `json:"snapshotContentName,omitempty"`

	// snapshotClassName records the VolumeSnapshotClass selected for the snapshot.
	// +optional
	SnapshotClassName string `json:"snapshotClassName,omitempty"`

	// sourceClonePVCName stores the PVC created in the template namespace before transfer.
	// +optional
	SourceClonePVCName string `json:"sourceClonePvcName,omitempty"`

	// sourceClonePVName stores the PV created from snapshot restore before transfer.
	// +optional
	SourceClonePVName string `json:"sourceClonePvName,omitempty"`

	// persistentVolumeClaimName is the PVC created in the clone namespace.
	// +optional
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`

	// requestedStorage records the requested storage capacity.
	// +optional
	RequestedStorage string `json:"requestedStorage,omitempty"`

	// storageClassName names the storage class to use.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// message provides human-readable status for the volume.
	// +optional
	Message string `json:"message,omitempty"`

	// phase reflects current progress for this volume.
	// +optional
	Phase CloneVolumePhase `json:"phase,omitempty"`
}

// ClonedVMStatus contains information about a cloned VM.
type ClonedVMStatus struct {
	// name is the name of the cloned VM.
	Name string `json:"name"`

	// ready indicates the VM is running and ready.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// message provides human-readable status.
	// +optional
	Message string `json:"message,omitempty"`
}

// CloneNetworkStatus records the state of the clone's network resources.
type CloneNetworkStatus struct {
	// vpcsCreated lists the KubeOVN VPC names created.
	// +optional
	VPCsCreated []string `json:"vpcsCreated,omitempty"`

	// subnetsCreated lists the KubeOVN Subnet names created.
	// +optional
	SubnetsCreated []string `json:"subnetsCreated,omitempty"`

	// egressGatewaysCreated lists the VpcEgressGateway names created.
	// +optional
	EgressGatewaysCreated []string `json:"egressGatewaysCreated,omitempty"`

	// ready indicates all network resources are available.
	Ready bool `json:"ready"`
}

// VirtualMachineCloneStatus defines the observed state of VirtualMachineClone.
type VirtualMachineCloneStatus struct {
	// phase is the overall clone phase.
	// +optional
	Phase ClonePhase `json:"phase,omitempty"`

	// cloneID is the unique CUID2 identifier for this clone's resources.
	// All resources are prefixed with this ID (e.g. "vm-abc123").
	// +optional
	CloneID string `json:"cloneID,omitempty"`

	// cloneNamespace is the namespace where cloned resources are created.
	// +optional
	CloneNamespace string `json:"cloneNamespace,omitempty"`

	// templateNamespace is the namespace where the template was found.
	// +optional
	TemplateNamespace string `json:"templateNamespace,omitempty"`

	// volumeStates tracks progress for each template volume being replicated.
	// +optional
	VolumeStates []CloneVolumeStatus `json:"volumeStates,omitempty"`

	// vmStatuses contains information about the cloned virtual machines.
	// +optional
	VMStatuses []ClonedVMStatus `json:"vmStatuses,omitempty"`

	// network records the state of network resources.
	// +optional
	Network *CloneNetworkStatus `json:"network,omitempty"`

	// startTime is when the clone started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime is when the clone finished.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// egressGatewayReady indicates whether the egress gateway is running.
	// +optional
	EgressGatewayReady bool `json:"egressGatewayReady,omitempty"`

	// ageAnchor is the resolved timestamp used to anchor watchdog age checks
	// for this clone's VMs. Populated by the controller in the Pending phase
	// from spec.replacesCloneID or spec.ageAnchor; copied onto each cloned
	// VM as the ruddervirt.io/age-anchor annotation.
	// +optional
	AgeAnchor *metav1.Time `json:"ageAnchor,omitempty"`

	// message is a human-readable summary of the clone state.
	// +optional
	Message string `json:"message,omitempty"`

	// conditions represent the latest available observations.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types for VirtualMachineClone.
const (
	CloneConditionTemplateValidated = "TemplateValidated"
	CloneConditionSnapshotSelected  = "SnapshotSelected"
	CloneConditionSnapshotsReady    = "SnapshotsReady"
	CloneConditionVolumesReady      = "VolumesReady"
	CloneConditionNetworkReady      = "NetworkReady"
	CloneConditionVMProvisioned     = "VMProvisioned"
	CloneConditionReady             = "Ready"
	CloneConditionAgeAnchorResolved = "AgeAnchorResolved"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vmc
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.templateName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Clone-ID",type=string,JSONPath=`.status.cloneID`
// +kubebuilder:printcolumn:name="Egress",type=boolean,JSONPath=`.status.egressGatewayReady`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualMachineClone is the Schema for the virtualmachineclones API.
// It clones VMs from a golden template namespace using CSI volume snapshots.
type VirtualMachineClone struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VirtualMachineClone.
	// +required
	Spec VirtualMachineCloneSpec `json:"spec"`

	// status defines the observed state of VirtualMachineClone.
	// +optional
	Status VirtualMachineCloneStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VirtualMachineCloneList contains a list of VirtualMachineClone.
type VirtualMachineCloneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VirtualMachineClone `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualMachineClone{}, &VirtualMachineCloneList{})
}
