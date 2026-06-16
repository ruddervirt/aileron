package build

import (
	"context"
	"fmt"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

// mockHandler returns a fixed next phase.
type mockHandler struct {
	next v1alpha1.BuildPhase
	err  error
}

func (m *mockHandler) Handle(_ context.Context, _ *v1alpha1.VirtualMachineBuild) (v1alpha1.BuildPhase, error) {
	return m.next, m.err
}

func TestStateMachine_Step_Terminal(t *testing.T) {
	sm := NewStateMachine(nil)

	for _, phase := range []v1alpha1.BuildPhase{v1alpha1.BuildPhaseSucceeded, v1alpha1.BuildPhaseFailed} {
		build := &v1alpha1.VirtualMachineBuild{}
		build.Status.Phase = phase

		got, err := sm.Step(context.Background(), build)
		if err != nil {
			t.Errorf("Step(%s) returned error: %v", phase, err)
		}
		if got != phase {
			t.Errorf("Step(%s) = %s, want %s", phase, got, phase)
		}
	}
}

func TestStateMachine_Step_EmptyPhase(t *testing.T) {
	sm := NewStateMachine(map[v1alpha1.BuildPhase]PhaseHandler{
		v1alpha1.BuildPhasePending: &mockHandler{next: v1alpha1.BuildPhaseNetworking},
	})

	build := &v1alpha1.VirtualMachineBuild{}
	// Empty phase should default to Pending.
	got, err := sm.Step(context.Background(), build)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != v1alpha1.BuildPhaseNetworking {
		t.Errorf("got %s, want Networking", got)
	}
}

func TestStateMachine_Step_NoHandler(t *testing.T) {
	sm := NewStateMachine(map[v1alpha1.BuildPhase]PhaseHandler{})

	build := &v1alpha1.VirtualMachineBuild{}
	build.Status.Phase = v1alpha1.BuildPhaseBuilding

	got, err := sm.Step(context.Background(), build)
	if err == nil {
		t.Fatal("expected error for missing handler")
	}
	if got != v1alpha1.BuildPhaseFailed {
		t.Errorf("got %s, want Failed", got)
	}
}

func TestStateMachine_Step_HandlerError(t *testing.T) {
	sm := NewStateMachine(map[v1alpha1.BuildPhase]PhaseHandler{
		v1alpha1.BuildPhasePending: &mockHandler{
			next: v1alpha1.BuildPhaseFailed,
			err:  fmt.Errorf("validation failed"),
		},
	})

	build := &v1alpha1.VirtualMachineBuild{}
	build.Status.Phase = v1alpha1.BuildPhasePending

	got, err := sm.Step(context.Background(), build)
	if err == nil {
		t.Fatal("expected error")
	}
	if got != v1alpha1.BuildPhaseFailed {
		t.Errorf("got %s, want Failed", got)
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		phase v1alpha1.BuildPhase
		want  bool
	}{
		{v1alpha1.BuildPhaseSucceeded, true},
		{v1alpha1.BuildPhaseFailed, true},
		{v1alpha1.BuildPhasePending, false},
		{v1alpha1.BuildPhaseBuilding, false},
	}
	for _, tt := range tests {
		if got := IsTerminal(tt.phase); got != tt.want {
			t.Errorf("IsTerminal(%s) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}

func TestIsVMTerminal(t *testing.T) {
	tests := []struct {
		phase v1alpha1.VMPhase
		want  bool
	}{
		{v1alpha1.VMPhaseSucceeded, true},
		{v1alpha1.VMPhaseFailed, true},
		{v1alpha1.VMPhasePending, false},
		{v1alpha1.VMPhaseProvisioning, false},
	}
	for _, tt := range tests {
		if got := IsVMTerminal(tt.phase); got != tt.want {
			t.Errorf("IsVMTerminal(%s) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}
