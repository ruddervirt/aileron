package build

import (
	"testing"
)

func TestParseNamespacedName(t *testing.T) {
	tests := []struct {
		input   string
		wantNS  string
		wantN   string
		wantErr bool
	}{
		{"images/my-dv", "images", "my-dv", false},
		{"default/output-vol", "default", "output-vol", false},
		{"no-slash", "", "", true},
		{"a/b/c", "a", "b/c", false}, // first slash wins
	}

	for _, tt := range tests {
		ns, name, err := parseNamespacedName(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseNamespacedName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if ns != tt.wantNS || name != tt.wantN {
			t.Errorf("parseNamespacedName(%q) = (%q, %q), want (%q, %q)", tt.input, ns, name, tt.wantNS, tt.wantN)
		}
	}
}
