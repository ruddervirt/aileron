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

// VirtualMachineNamespaceSpec defines the desired state of a VirtualMachineNamespace.
// A VirtualMachineNamespace is a logical grouping of isolated resources (VMs, networks,
// storage) within a single Kubernetes namespace. It replaces per-build Kubernetes
// namespaces, providing isolation without cross-namespace complexity.
type VirtualMachineNamespaceSpec struct {
	// ownerRef identifies the CR that owns this namespace (e.g., a VirtualMachineBuild or VirtualMachineClone).
	// +optional
	OwnerRef *VirtualMachineNamespaceOwnerRef `json:"ownerRef,omitempty"`
}

// VirtualMachineNamespaceOwnerRef identifies the CR that created this namespace.
type VirtualMachineNamespaceOwnerRef struct {
	// kind is the CRD kind (e.g., "VirtualMachineBuild", "VirtualMachineClone").
	Kind string `json:"kind"`

	// name is the CR name.
	Name string `json:"name"`

	// namespace is the CR namespace.
	Namespace string `json:"namespace"`
}

// VirtualMachineNamespaceStatus defines the observed state of a VirtualMachineNamespace.
type VirtualMachineNamespaceStatus struct {
	// phase indicates the lifecycle phase.
	// +optional
	Phase VirtualMachineNamespacePhase `json:"phase,omitempty"`

	// vms lists the VirtualMachines in this namespace.
	// +optional
	VMs []VirtualMachineNamespaceVM `json:"vms,omitempty"`

	// network contains the network topology state.
	// +optional
	Network *VirtualMachineNamespaceNetwork `json:"network,omitempty"`
}

// VirtualMachineNamespacePhase represents the lifecycle phase of a VirtualMachineNamespace.
// +kubebuilder:validation:Enum=Active;Deleting
type VirtualMachineNamespacePhase string

const (
	VMNamespacePhaseActive   VirtualMachineNamespacePhase = "Active"
	VMNamespacePhaseDeleting VirtualMachineNamespacePhase = "Deleting"
)

// VirtualMachineNamespaceVM describes a VM within the namespace.
type VirtualMachineNamespaceVM struct {
	// name is the VM name.
	Name string `json:"name"`

	// pvcName is the PVC backing this VM's disk.
	// +optional
	PVCName string `json:"pvcName,omitempty"`
}

// VirtualMachineNamespaceNetwork describes the network topology.
type VirtualMachineNamespaceNetwork struct {
	// vpcs lists the KubeOVN VPC names.
	// +optional
	VPCs []string `json:"vpcs,omitempty"`

	// subnets lists the KubeOVN Subnet names.
	// +optional
	Subnets []string `json:"subnets,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.ownerRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualMachineNamespace is a logical grouping of isolated VMs, networks, and storage
// resources within a single Kubernetes namespace. It provides the isolation boundaries
// that were previously achieved through per-build Kubernetes namespaces, without the
// cross-namespace complexity.
type VirtualMachineNamespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualMachineNamespaceSpec   `json:"spec,omitempty"`
	Status VirtualMachineNamespaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VirtualMachineNamespaceList contains a list of VirtualMachineNamespace.
type VirtualMachineNamespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachineNamespace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualMachineNamespace{}, &VirtualMachineNamespaceList{})
}
