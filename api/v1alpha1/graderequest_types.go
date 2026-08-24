// SPDX-License-Identifier: GPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GradeRequestPhase represents the current phase of a grade request.
// +kubebuilder:validation:Enum=Pending;Running;Ready;Failed
type GradeRequestPhase string

const (
	GradeRequestPhasePending GradeRequestPhase = "Pending"
	GradeRequestPhaseRunning GradeRequestPhase = "Running"
	GradeRequestPhaseReady   GradeRequestPhase = "Ready"
	GradeRequestPhaseFailed  GradeRequestPhase = "Failed"
)

// GradeVM defines a single VM target within a grade request. The grading
// method (serial-Windows vs serial-Linux) is resolved by the controller from
// the VM's ruddervirt.io/os label, so callers don't specify it.
type GradeVM struct {
	// name is the full KubeVirt VirtualMachine name to grade.
	// +required
	Name string `json:"name"`
	// commands are executed sequentially on the VM's serial console.
	// +required
	Commands []string `json:"commands"`
	// user is the serial console login username.
	// +required
	User string `json:"user"`
	// password is the serial console login password.
	// +required
	Password string `json:"password"`
	// domain is the optional SAC domain for domain-joined Windows guests.
	// +optional
	Domain string `json:"domain,omitempty"`
}

// GradeRequestSpec defines the desired state of a grade request.
// Use the VMs field for multi-VM grading. The legacy single-VM fields
// (VMName, Commands, User, Password, Domain) are still accepted for backward
// compatibility and are normalized into a one-element VMs slice by the
// controller.
type GradeRequestSpec struct {
	// namespace is the Kubernetes namespace containing the target VMs.
	// +required
	Namespace string `json:"namespace"`
	// vms is the list of VMs to grade.
	// +optional
	VMs []GradeVM `json:"vms,omitempty"`

	// Legacy single-VM fields (kept for backward compatibility).
	// +optional
	VMName string `json:"vmName,omitempty"`
	// +optional
	Commands []string `json:"commands,omitempty"`
	// +optional
	User string `json:"user,omitempty"`
	// +optional
	Password string `json:"password,omitempty"`
	// +optional
	Domain string `json:"domain,omitempty"`
}

// GradeCommandResult contains the result of a single command execution.
type GradeCommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int32  `json:"exitCode"`
}

// GradeVMStatus tracks the status of grading a single VM.
type GradeVMStatus struct {
	Name string `json:"name"`
	// +optional
	Phase GradeRequestPhase `json:"phase,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Results []GradeCommandResult `json:"results,omitempty"`

	// autoStarted is true when the operator powered this VM on because it was
	// off at grade time. Only auto-started VMs are powered back off after
	// grading; VMs that were already running are left running.
	// +optional
	AutoStarted bool `json:"autoStarted,omitempty"`
	// bootStartedAt records when the operator issued the power-on, so the
	// post-boot grace period can be enforced across reconciles.
	// +optional
	BootStartedAt *metav1.Time `json:"bootStartedAt,omitempty"`
	// poweredOff is true once the operator has powered an auto-started VM
	// back off (or decided to leave it running), making power-off idempotent.
	// +optional
	PoweredOff bool `json:"poweredOff,omitempty"`

	// queuePosition is the VM's 1-based place in the global grading queue while
	// it waits for a free concurrency slot. It is set only while the VM is
	// queued (not yet powered on / graded) and cleared once the VM is admitted.
	// +optional
	QueuePosition *int32 `json:"queuePosition,omitempty"`
}

// GradeRequestStatus defines the observed state of a grade request.
type GradeRequestStatus struct {
	// +optional
	Phase GradeRequestPhase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	VMStatuses []GradeVMStatus `json:"vmStatuses,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// activeSlots is the number of grading concurrency slots occupied
	// cluster-wide (any VM booting-for-grade or with a running grade job) as
	// observed on the last reconcile. Surfaced so callers can render queue
	// state without recomputing it.
	// +optional
	ActiveSlots int32 `json:"activeSlots,omitempty"`
	// maxSlots is the configured maximum number of concurrent grades (0 =
	// unlimited).
	// +optional
	MaxSlots int32 `json:"maxSlots,omitempty"`
	// queuedCount is the number of this request's VMs currently waiting for a
	// free grading slot.
	// +optional
	QueuedCount int32 `json:"queuedCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gr
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GradeRequest is the Schema for the graderequests API. It executes commands
// on one or more VMs via their serial console and records the results.
type GradeRequest struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec GradeRequestSpec `json:"spec,omitempty"`

	// +optional
	Status GradeRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GradeRequestList contains a list of GradeRequest.
type GradeRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GradeRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GradeRequest{}, &GradeRequestList{})
}
