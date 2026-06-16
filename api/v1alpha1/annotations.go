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

// AnnotationOrigin is an optional, caller-provided attribution string. When a
// caller sets it on a VirtualMachineBuild or VirtualMachineClone, aileron
// propagates it verbatim onto the VirtualMachine/VirtualMachineInstance it
// creates, so an external watcher can correlate VM lifecycle events back to the
// originating request. aileron never interprets the value.
const AnnotationOrigin = "ruddervirt.io/origin"

// Grade job labels stamp the grader Job (and its pod template) with the
// GradeRequest name and its target VM/namespace, so the controller can locate a
// running grade job for a given VM.
const (
	LabelGradeRequest  = "ruddervirt.io/grade-request"
	LabelGradeTargetVM = "ruddervirt.io/target-vm"
	LabelGradeTargetNS = "ruddervirt.io/target-namespace"
)
