package ws

import (
	"errors"
	"strings"
	"testing"
)

func TestDetectGuestOS(t *testing.T) {
	tests := []struct {
		name     string
		snapshot string
		want     string
	}{
		// The real snapshot from the ns-yblv4po3ynw08q5-client grade failure.
		{"sac prompt", "\r\nSAC>\r\nSAC>", "windows"},
		{"sac with nuls", "\x00S\x00AC> ", "windows"},
		{"windows version banner", "Microsoft Windows [Version 10.0.20348.1]", "windows"},
		{"linux login", "\r\nlocalhost login: ", "linux"},
		{"linux none login", "(none) login: ", "linux"},
		{"linux banner", "Debian GNU/Linux 12", "linux"},
		{"shell prompt only is ambiguous", "root@host:~# ", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectGuestOS(tt.snapshot); got != tt.want {
				t.Fatalf("detectGuestOS(%q) = %q, want %q", tt.snapshot, got, tt.want)
			}
		})
	}
}

func TestAnnotateOSMismatch(t *testing.T) {
	base := errors.New("timed out waiting for pattern")

	t.Run("linux method against windows guest is flagged", func(t *testing.T) {
		got := annotateOSMismatch(base, "\r\nSAC>\r\nSAC>", "linux")
		if !strings.Contains(got.Error(), "guest looks like windows") ||
			!strings.Contains(got.Error(), "ruddervirt.io/os label") {
			t.Fatalf("expected mismatch hint, got: %v", got)
		}
		if !errors.Is(got, base) {
			t.Fatalf("wrapped error must preserve the cause")
		}
	})

	t.Run("windows method against linux guest is flagged", func(t *testing.T) {
		got := annotateOSMismatch(base, "localhost login: ", "windows")
		if !strings.Contains(got.Error(), "guest looks like linux") {
			t.Fatalf("expected mismatch hint, got: %v", got)
		}
	})

	t.Run("matching OS is left unchanged", func(t *testing.T) {
		if got := annotateOSMismatch(base, "\r\nSAC>", "windows"); got != base {
			t.Fatalf("expected unchanged error, got: %v", got)
		}
	})

	t.Run("no tell is left unchanged", func(t *testing.T) {
		if got := annotateOSMismatch(base, "some ambiguous noise", "linux"); got != base {
			t.Fatalf("expected unchanged error, got: %v", got)
		}
	})

	t.Run("nil stays nil", func(t *testing.T) {
		if got := annotateOSMismatch(nil, "\r\nSAC>", "linux"); got != nil {
			t.Fatalf("expected nil, got: %v", got)
		}
	})
}
