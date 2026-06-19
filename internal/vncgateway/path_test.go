package vncgateway

import "testing"

func TestValidName(t *testing.T) {
	for _, ok := range []string{"abc123", "a", "build-id-1", "x9", "abc-def-123"} {
		if !validName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "..", "a/b", "UPPER", "-leading", "trailing-", "has space", "q?x"} {
		if validName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestParseTwoSegments(t *testing.T) {
	tests := []struct {
		path   string
		wantA  string
		wantB  string
		wantOK bool
	}{
		{"/internal/bid/vm", "bid", "vm", true},
		{"/internal/bid/vm/", "bid", "vm", true},
		{"/internal/bid", "", "", false},
		{"/internal/bid/vm/extra", "", "", false},
		{"/other/bid/vm", "", "", false},
		{"/internal/../etc", "", "", false},
	}
	for _, tt := range tests {
		a, b, ok := parseTwoSegments(tt.path, "/internal/")
		if ok != tt.wantOK || a != tt.wantA || b != tt.wantB {
			t.Errorf("parseTwoSegments(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tt.path, a, b, ok, tt.wantA, tt.wantB, tt.wantOK)
		}
	}
}
