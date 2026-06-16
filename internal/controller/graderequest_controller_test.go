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

package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

func TestShortVMHashDeterministic(t *testing.T) {
	got1 := shortVMHash("ns-y3lf3zxvvsefj7f-workstation")
	got2 := shortVMHash("ns-y3lf3zxvvsefj7f-workstation")
	if got1 != got2 {
		t.Fatalf("shortVMHash not deterministic: %q vs %q", got1, got2)
	}
	if len(got1) != 8 {
		t.Fatalf("shortVMHash length = %d, want 8", len(got1))
	}
}

func TestShortVMHashDistinguishesVMs(t *testing.T) {
	a := shortVMHash("ns-y3lf3zxvvsefj7f-workstation")
	b := shortVMHash("ns-y3lf3zxvvsefj7f-server")
	if a == b {
		t.Fatalf("shortVMHash collided for different VM names: %q", a)
	}
}

func TestFilterGradableVMs(t *testing.T) {
	client := v1alpha1.GradeVM{Name: "ns-x-client", Commands: []string{"echo hi"}}
	server := v1alpha1.GradeVM{Name: "ns-x-server", Commands: []string{}}

	t.Run("drops empty-command VMs and keeps the rest", func(t *testing.T) {
		gradable, reason := filterGradableVMs([]v1alpha1.GradeVM{client, server})
		if reason != "" {
			t.Fatalf("reason = %q, want empty", reason)
		}
		if len(gradable) != 1 || gradable[0].Name != client.Name {
			t.Fatalf("gradable = %+v, want only %q", gradable, client.Name)
		}
	})

	t.Run("fails when no VM has commands", func(t *testing.T) {
		_, reason := filterGradableVMs([]v1alpha1.GradeVM{server})
		if reason == "" {
			t.Fatal("expected failure reason when no VM has commands")
		}
	})

	t.Run("rejects a commanded VM with no name", func(t *testing.T) {
		_, reason := filterGradableVMs([]v1alpha1.GradeVM{{Name: "", Commands: []string{"echo hi"}}})
		if !strings.Contains(reason, "must have a name") {
			t.Fatalf("reason = %q, want name error", reason)
		}
	})
}

func TestGradeVMIsOff(t *testing.T) {
	cases := []struct {
		phase string
		want  bool
	}{
		{"", true},          // no VMI: VM is stopped
		{"Succeeded", true}, // guest shut down, VMI lingers
		{"Failed", true},    // guest crashed, VMI lingers
		{"Running", false},
		{"Pending", false},
		{"Scheduling", false},
		{"Scheduled", false},
	}
	for _, tc := range cases {
		if got := gradeVMIsOff(tc.phase); got != tc.want {
			t.Errorf("gradeVMIsOff(%q) = %v, want %v", tc.phase, got, tc.want)
		}
	}
}

func TestGradeBootGateReady(t *testing.T) {
	now := time.Now()
	recent := metav1.NewTime(now.Add(-10 * time.Second))
	old := metav1.NewTime(now.Add(-2 * time.Minute))

	cases := []struct {
		name          string
		phase         string
		autoStarted   bool
		bootStartedAt *metav1.Time
		want          bool
	}{
		{"already-running VM is ready immediately", "Running", false, nil, true},
		{"auto-started VM still in grace period", "Running", true, &recent, false},
		{"auto-started VM past grace period", "Running", true, &old, true},
		{"auto-started VM not yet Running", "Scheduling", true, &recent, false},
		{"auto-started VM past grace but not Running", "Scheduled", true, &old, false},
		{"VM off is never ready", "", false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gradeBootGateReady(tc.phase, tc.autoStarted, tc.bootStartedAt, now, 90*time.Second)
			if got != tc.want {
				t.Fatalf("gradeBootGateReady = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGradeBootTimedOut(t *testing.T) {
	now := time.Now()
	recent := metav1.NewTime(now.Add(-1 * time.Minute))
	old := metav1.NewTime(now.Add(-10 * time.Minute))

	cases := []struct {
		name          string
		phase         string
		autoStarted   bool
		bootStartedAt *metav1.Time
		want          bool
	}{
		{"not auto-started never times out", "Scheduling", false, nil, false},
		{"within timeout", "Scheduling", true, &recent, false},
		{"past timeout and not Running", "Scheduling", true, &old, true},
		{"past timeout but Running", "Running", true, &old, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gradeBootTimedOut(tc.phase, tc.autoStarted, tc.bootStartedAt, now, gradeBootTimeout)
			if got != tc.want {
				t.Fatalf("gradeBootTimedOut = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGradeBootWaitDuration(t *testing.T) {
	t.Run("defaults to 90s", func(t *testing.T) {
		t.Setenv("GRADER_BOOT_WAIT_SECONDS", "")
		if got := gradeBootWaitDuration(); got != 90*time.Second {
			t.Fatalf("gradeBootWaitDuration() = %v, want 90s", got)
		}
	})

	t.Run("env override", func(t *testing.T) {
		t.Setenv("GRADER_BOOT_WAIT_SECONDS", "30")
		if got := gradeBootWaitDuration(); got != 30*time.Second {
			t.Fatalf("gradeBootWaitDuration() = %v, want 30s", got)
		}
	})

	t.Run("garbage falls back to default", func(t *testing.T) {
		t.Setenv("GRADER_BOOT_WAIT_SECONDS", "not-a-number")
		if got := gradeBootWaitDuration(); got != 90*time.Second {
			t.Fatalf("gradeBootWaitDuration() = %v, want 90s", got)
		}
	})
}

func TestBuildJobForVMNameUnder63Chars(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1 to scheme: %v", err)
	}
	r := &GradeRequestReconciler{Scheme: scheme}

	// gr.Name is bounded by our generateName scheme ("grade-{ns}-{5-char}");
	// the bug we're guarding against was an arbitrarily long vm.Name pushing
	// the combined job name past 63. The hash suffix should keep job names
	// bounded regardless of vm.Name length.
	cases := []struct {
		name   string
		grName string
		vmName string
	}{
		{"typical", "grade-ns-y3lf3zxvvsefj7f-2cfkh", "ns-y3lf3zxvvsefj7f-workstation"},
		{"long vm name", "grade-ns-y3lf3zxvvsefj7f-2cfkh", strings.Repeat("a", 200)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gr := &v1alpha1.GradeRequest{
				ObjectMeta: metav1.ObjectMeta{Name: tc.grName, Namespace: "ruddervirt-system"},
			}
			vm := &v1alpha1.GradeVM{Name: tc.vmName, Commands: []string{"echo hi"}}

			job, err := r.buildJobForVM(gr, vm, "SERIAL_LINUX")
			if err != nil {
				t.Fatalf("buildJobForVM error: %v", err)
			}
			if len(job.Name) > 63 {
				t.Fatalf("job name %q is %d chars, must be <= 63", job.Name, len(job.Name))
			}
			if !strings.HasPrefix(job.Name, tc.grName+"-") {
				t.Fatalf("job name %q should start with %q-", job.Name, tc.grName)
			}
		})
	}
}
