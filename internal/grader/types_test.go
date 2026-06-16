package grader

import "testing"

func TestParseGradeMethod(t *testing.T) {
	tests := []struct {
		input   string
		want    GradeMethod
		wantErr bool
	}{
		{"SERIAL_WINDOWS", GradeMethodSerialWindows, false},
		{"SERIAL_LINUX", GradeMethodSerialLinux, false},
		{"", 0, true},
		{"SSH", 0, true},
		{"unknown", 0, true},
		{"serial_linux", 0, true}, // case-sensitive
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseGradeMethod(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseGradeMethod(%q) expected error, got %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseGradeMethod(%q) unexpected error: %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("ParseGradeMethod(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestGradeMethod_String(t *testing.T) {
	tests := []struct {
		method GradeMethod
		want   string
	}{
		{GradeMethodSerialWindows, "SERIAL_WINDOWS"},
		{GradeMethodSerialLinux, "SERIAL_LINUX"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.method.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
