package build

import (
	"encoding/json"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

// TestBuildHookSidecarsAnnotation_FloppyArg confirms the --floppy sidecar arg
// is present iff floppyEnabled is true — this is the primary, positive gate
// that keeps a clone from ever injecting a floppy device (clones regenerate
// their own hook annotation via internal/clone's hookSidecarsJSON, which
// never sets this arg).
func TestBuildHookSidecarsAnnotation_FloppyArg(t *testing.T) {
	vmSpec := &v1alpha1.BuildVM{
		Name:   "installer",
		Floppy: &v1alpha1.Floppy{Files: []v1alpha1.FloppyFileRef{{Name: "Autounattend.xml"}}},
	}

	for _, floppyEnabled := range []bool{true, false} {
		got, err := BuildHookSidecarsAnnotation("bld123", vmSpec, floppyEnabled)
		if err != nil {
			t.Fatalf("floppyEnabled=%v: %v", floppyEnabled, err)
		}

		var hooks []map[string]any
		if err := json.Unmarshal([]byte(got), &hooks); err != nil {
			t.Fatalf("floppyEnabled=%v: invalid JSON: %v", floppyEnabled, err)
		}
		args, _ := hooks[0]["args"].([]any)
		hasFloppy := false
		for _, a := range args {
			if a == "--floppy" {
				hasFloppy = true
			}
		}
		if hasFloppy != floppyEnabled {
			t.Errorf("floppyEnabled=%v: --floppy present in args = %v, want %v (args=%v)", floppyEnabled, hasFloppy, floppyEnabled, args)
		}
	}
}

// TestBuildHookSidecarsAnnotation_NoHookWithoutEFIOrFloppy confirms no hook
// annotation is emitted when neither EFIFirmware nor Floppy is configured.
func TestBuildHookSidecarsAnnotation_NoHookWithoutEFIOrFloppy(t *testing.T) {
	vmSpec := &v1alpha1.BuildVM{Name: "plain"}

	got, err := BuildHookSidecarsAnnotation("bld123", vmSpec, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty hook annotation, got: %s", got)
	}
}
