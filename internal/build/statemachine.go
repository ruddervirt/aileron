package build

import (
	"context"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

// PhaseHandler executes the logic for a single build phase and returns
// the next phase to transition to, or an error.
type PhaseHandler interface {
	Handle(ctx context.Context, build *v1alpha1.VirtualMachineBuild) (next v1alpha1.BuildPhase, err error)
}

// VMPhaseHandler executes the logic for a single VM's phase within a build.
type VMPhaseHandler interface {
	HandleVM(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) (next v1alpha1.VMPhase, err error)
}

// StateMachine drives a VirtualMachineBuild through its overall lifecycle phases.
type StateMachine struct {
	handlers map[v1alpha1.BuildPhase]PhaseHandler
}

// NewStateMachine creates a StateMachine with the given phase handlers.
func NewStateMachine(handlers map[v1alpha1.BuildPhase]PhaseHandler) *StateMachine {
	return &StateMachine{handlers: handlers}
}

// Step executes the handler for the build's current phase and returns the next phase.
func (sm *StateMachine) Step(ctx context.Context, build *v1alpha1.VirtualMachineBuild) (v1alpha1.BuildPhase, error) {
	phase := build.Status.Phase
	if phase == "" {
		phase = v1alpha1.BuildPhasePending
	}

	if IsTerminal(phase) {
		return phase, nil
	}

	handler, ok := sm.handlers[phase]
	if !ok {
		return v1alpha1.BuildPhaseFailed, fmt.Errorf("no handler registered for phase %q", phase)
	}

	return handler.Handle(ctx, build)
}

// Overall build phase transitions.
var Transitions = map[v1alpha1.BuildPhase][]v1alpha1.BuildPhase{
	v1alpha1.BuildPhasePending:              {v1alpha1.BuildPhaseNetworking, v1alpha1.BuildPhaseBuilding, v1alpha1.BuildPhaseFailed},
	v1alpha1.BuildPhaseNetworking:           {v1alpha1.BuildPhaseBuilding, v1alpha1.BuildPhaseFailed},
	v1alpha1.BuildPhaseBuilding:             {v1alpha1.BuildPhaseCapturingDisks, v1alpha1.BuildPhaseFailed},
	v1alpha1.BuildPhaseCapturingDisks:       {v1alpha1.BuildPhaseExporting, v1alpha1.BuildPhaseTemplateProvisioning, v1alpha1.BuildPhaseFailed},
	v1alpha1.BuildPhaseExporting:            {v1alpha1.BuildPhaseTemplateProvisioning, v1alpha1.BuildPhaseFailed},
	v1alpha1.BuildPhaseTemplateProvisioning: {v1alpha1.BuildPhaseSucceeded, v1alpha1.BuildPhaseFailed},
}

// Per-VM phase transitions.
var VMTransitions = map[v1alpha1.VMPhase][]v1alpha1.VMPhase{
	v1alpha1.VMPhasePending:         {v1alpha1.VMPhaseSourceImporting, v1alpha1.VMPhaseFailed},
	v1alpha1.VMPhaseSourceImporting: {v1alpha1.VMPhaseBooting, v1alpha1.VMPhaseFailed},
	v1alpha1.VMPhaseBooting:         {v1alpha1.VMPhaseBootCommand, v1alpha1.VMPhaseProvisioning, v1alpha1.VMPhaseFailed},
	v1alpha1.VMPhaseBootCommand:     {v1alpha1.VMPhaseProvisioning, v1alpha1.VMPhaseFailed},
	v1alpha1.VMPhaseProvisioning:    {v1alpha1.VMPhaseShuttingDown, v1alpha1.VMPhaseFailed},
	v1alpha1.VMPhaseShuttingDown:    {v1alpha1.VMPhaseDiskCaptured, v1alpha1.VMPhaseFailed},
	v1alpha1.VMPhaseDiskCaptured:    {v1alpha1.VMPhaseSucceeded, v1alpha1.VMPhaseFailed},
}

// IsTerminal returns true if the overall build phase is terminal.
func IsTerminal(phase v1alpha1.BuildPhase) bool {
	return phase == v1alpha1.BuildPhaseSucceeded || phase == v1alpha1.BuildPhaseFailed
}

// IsVMTerminal returns true if a VM phase is terminal.
func IsVMTerminal(phase v1alpha1.VMPhase) bool {
	return phase == v1alpha1.VMPhaseSucceeded || phase == v1alpha1.VMPhaseFailed
}
