// SPDX-License-Identifier: GPL-3.0-only

package v1alpha1

// AnnotationOrigin is an optional, caller-provided attribution string. When a
// caller sets it on a VirtualMachineBuild or VirtualMachineClone, aileron
// propagates it verbatim onto the VirtualMachine/VirtualMachineInstance it
// creates, so an external watcher can correlate VM lifecycle events back to the
// originating request. aileron never interprets the value.
const AnnotationOrigin = "ruddervirt.io/origin"

// AnnotationAgeAnchor overrides a cloned VM's effective creation time for
// watchdog age checks. Stamped by the clone controller when a
// VirtualMachineClone inherits its age from a predecessor (see
// spec.replacesCloneID / spec.ageAnchor).
const AnnotationAgeAnchor = "ruddervirt.io/age-anchor"

// AnnotationExpiresAt is the absolute RFC3339 timestamp after which a cloned
// VM is eligible for watchdog deletion (see VirtualMachineCloneStatus.ExpiresAt).
// Stamped by aileron at VM-creation time; the watchdog treats it as the
// authoritative, opt-in deletion trigger instead of inferring eligibility
// from creationTimestamp plus a fleet-wide max age.
const AnnotationExpiresAt = "ruddervirt.io/expires-at"

// AnnotationInvisible marks a VM as excluded from the console/VNC-access UI.
// Stamped "true" by the build controller (internal/build/vm.go buildVM) only
// when BuildVM.Invisible is true — never stamped "false" or removed
// explicitly. It survives into the golden template VM because
// TemplateProvisioner.convertToTemplate merges (not replaces) VM-level
// annotations, and survives into every cloned VM because
// clone.ensureVirtualMachine DeepCopies the template VM's annotations
// unchanged. Consumed by internal/clone/volume.go (CheckVMsReady) to
// populate ClonedVMStatus.Invisible, and by internal/ui/server.go to
// exclude the VM from a build's or clone's Consoles list.
const AnnotationInvisible = "ruddervirt.io/invisible"

// Grade job labels stamp the grader Job (and its pod template) with the
// GradeRequest name and its target VM/namespace, so the controller can locate a
// running grade job for a given VM.
const (
	LabelGradeRequest  = "ruddervirt.io/grade-request"
	LabelGradeTargetVM = "ruddervirt.io/target-vm"
	LabelGradeTargetNS = "ruddervirt.io/target-namespace"
)
