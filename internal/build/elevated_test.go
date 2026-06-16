package build

import (
	"encoding/base64"
	"reflect"
	"testing"
)

func TestParseElevatedStatus(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantState string
		wantOff   int64
		wantExit  *int
	}{
		{
			name:      "running",
			line:      "__AILERON_STATUS__ state=running offset=1234",
			wantState: "running",
			wantOff:   1234,
			wantExit:  nil,
		},
		{
			name:      "completed exit 0",
			line:      "__AILERON_STATUS__ state=completed offset=4096 exit=0",
			wantState: "completed",
			wantOff:   4096,
			wantExit:  new(0),
		},
		{
			name:      "completed exit 101 sentinel",
			line:      "__AILERON_STATUS__ state=completed offset=8192 exit=101",
			wantState: "completed",
			wantOff:   8192,
			wantExit:  new(101),
		},
		{
			name:      "fields out of order",
			line:      "__AILERON_STATUS__ exit=5 offset=42 state=completed",
			wantState: "completed",
			wantOff:   42,
			wantExit:  new(5),
		},
		{
			name:      "completed exit 252 not-elevated sentinel",
			line:      "__AILERON_STATUS__ state=completed offset=512 exit=252",
			wantState: "completed",
			wantOff:   512,
			wantExit:  new(252),
		},
		{
			name:      "completed exit 267011 never-ran",
			line:      "__AILERON_STATUS__ state=completed offset=0 exit=267011",
			wantState: "completed",
			wantOff:   0,
			wantExit:  new(267011),
		},
		{
			name:      "missing marker defaults to running",
			line:      "garbage line",
			wantState: "running",
			wantOff:   0,
			wantExit:  nil,
		},
		{
			name:      "empty line defaults to running",
			line:      "",
			wantState: "running",
			wantOff:   0,
			wantExit:  nil,
		},
		{
			name:      "unparseable field tolerated",
			line:      "__AILERON_STATUS__ state=running offset=NaN nonsense=x",
			wantState: "running",
			wantOff:   0,
			wantExit:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, off, exit := parseElevatedStatus(tc.line)
			if state != tc.wantState {
				t.Errorf("state: got %q want %q", state, tc.wantState)
			}
			if off != tc.wantOff {
				t.Errorf("offset: got %d want %d", off, tc.wantOff)
			}
			if !reflect.DeepEqual(exit, tc.wantExit) {
				t.Errorf("exit: got %v want %v", deref(exit), deref(tc.wantExit))
			}
		})
	}
}

func TestSplitPollOutput(t *testing.T) {
	t.Run("emits log lines and parses marker", func(t *testing.T) {
		out := "[elevated] task started\r\nhello\r\nworld\r\n__AILERON_STATUS__ state=completed offset=64 exit=0\r\n"
		var seen []string
		state, off, exit := splitPollOutput(out, 0, func(s string) { seen = append(seen, s) })

		want := []string{"[elevated] task started", "hello", "world"}
		if !reflect.DeepEqual(seen, want) {
			t.Errorf("lines: got %#v want %#v", seen, want)
		}
		if state != "completed" || off != 64 || exit == nil || *exit != 0 {
			t.Errorf("status: state=%q off=%d exit=%v", state, off, deref(exit))
		}
	})

	t.Run("blank lines filtered, no marker yields running", func(t *testing.T) {
		out := "first\n\n   \nsecond\n"
		var seen []string
		state, off, exit := splitPollOutput(out, 0, func(s string) { seen = append(seen, s) })

		want := []string{"first", "second"}
		if !reflect.DeepEqual(seen, want) {
			t.Errorf("lines: got %#v want %#v", seen, want)
		}
		if state != "running" || off != 0 || exit != nil {
			t.Errorf("status: state=%q off=%d exit=%v", state, off, deref(exit))
		}
	})

	t.Run("marker-only output emits nothing", func(t *testing.T) {
		out := "__AILERON_STATUS__ state=running offset=0\n"
		var seen []string
		state, off, exit := splitPollOutput(out, 0, func(s string) { seen = append(seen, s) })

		if len(seen) != 0 {
			t.Errorf("expected no lines, got %#v", seen)
		}
		if state != "running" || off != 0 || exit != nil {
			t.Errorf("status: state=%q off=%d exit=%v", state, off, deref(exit))
		}
	})

	t.Run("missing marker preserves prior offset", func(t *testing.T) {
		out := "lone line with no marker\n"
		var seen []string
		state, off, exit := splitPollOutput(out, 4096, func(s string) { seen = append(seen, s) })

		want := []string{"lone line with no marker"}
		if !reflect.DeepEqual(seen, want) {
			t.Errorf("lines: got %#v want %#v", seen, want)
		}
		if state != elevatedStateRunning || off != 4096 || exit != nil {
			t.Errorf("status: state=%q off=%d exit=%v", state, off, deref(exit))
		}
	})
}

func TestEncodeElevatedEnv(t *testing.T) {
	t.Run("empty map yields empty string", func(t *testing.T) {
		if got := encodeElevatedEnv(nil); got != "" {
			t.Errorf("nil map: got %q want empty", got)
		}
		if got := encodeElevatedEnv(map[string]string{}); got != "" {
			t.Errorf("empty map: got %q want empty", got)
		}
	})

	t.Run("round-trips sorted NAME=VALUE lines", func(t *testing.T) {
		env := map[string]string{
			"ZED":    "last",
			"APP":    "has = equals = signs",
			"QUOTED": "it's got 'single quotes'",
			"SPACES": "  padded  ",
		}
		decoded, err := base64.StdEncoding.DecodeString(encodeElevatedEnv(env))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := "APP=has = equals = signs\n" +
			"QUOTED=it's got 'single quotes'\n" +
			"SPACES=  padded  \n" +
			"ZED=last\n"
		if string(decoded) != want {
			t.Errorf("decoded:\n%q\nwant:\n%q", decoded, want)
		}
	})

	t.Run("deterministic across calls", func(t *testing.T) {
		env := map[string]string{"B": "2", "A": "1", "C": "3"}
		first := encodeElevatedEnv(env)
		for range 10 {
			if got := encodeElevatedEnv(env); got != first {
				t.Fatalf("non-deterministic encoding: %q vs %q", got, first)
			}
		}
	})
}

func TestDescribeElevatedExit(t *testing.T) {
	if err := describeElevatedExit(elevatedExitNotElevated); err == nil {
		t.Error("exit 252: want descriptive error, got nil")
	}
	if err := describeElevatedExit(schedNeverRan); err == nil {
		t.Error("exit 267011: want descriptive error, got nil")
	}
	for _, code := range []int{0, 1, 101, 251, 253} {
		if err := describeElevatedExit(code); err != nil {
			t.Errorf("exit %d: want nil, got %v", code, err)
		}
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
