package build

import (
	"encoding/json"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

// TestBuildHookSidecarsAnnotation_NeverEmitsFloppyArg confirms args is always
// exactly ["--version","v1alpha2"], with or without vmSpec.Floppy set.
// KubeVirt's sidecar-shim only recognizes --version on its own command line
// and rejects any other flag before ever exec'ing the hook binary, so a
// --floppy arg here would crash-loop every build using it (see
// cmd/sidecar/main.go — floppy injection is gated by file presence instead).
func TestBuildHookSidecarsAnnotation_NeverEmitsFloppyArg(t *testing.T) {
	vmSpec := &v1alpha1.BuildVM{
		Name:   "installer",
		Floppy: &v1alpha1.Floppy{Files: []v1alpha1.FloppyFileRef{{Name: "Autounattend.xml"}}},
	}

	got, err := BuildHookSidecarsAnnotation("bld123", vmSpec)
	if err != nil {
		t.Fatal(err)
	}

	var hooks []map[string]any
	if err := json.Unmarshal([]byte(got), &hooks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	args, _ := hooks[0]["args"].([]any)
	want := []any{"--version", "v1alpha2"}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// TestBuildHookSidecarsAnnotation_NoHookWithoutEFIOrFloppy confirms no hook
// annotation is emitted when neither EFIFirmware nor Floppy is configured.
func TestBuildHookSidecarsAnnotation_NoHookWithoutEFIOrFloppy(t *testing.T) {
	vmSpec := &v1alpha1.BuildVM{Name: "plain"}

	got, err := BuildHookSidecarsAnnotation("bld123", vmSpec)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty hook annotation, got: %s", got)
	}
}
