package grader

import "fmt"

// GradeMethod represents the method used to connect to a VM for grading.
type GradeMethod int

const (
	GradeMethodSerialWindows GradeMethod = iota
	GradeMethodSerialLinux
)

// ParseGradeMethod resolves the method string from a grade request. An empty
// or unknown value is a misconfiguration — callers must set one of the
// supported methods explicitly.
func ParseGradeMethod(s string) (GradeMethod, error) {
	switch s {
	case "SERIAL_WINDOWS":
		return GradeMethodSerialWindows, nil
	case "SERIAL_LINUX":
		return GradeMethodSerialLinux, nil
	default:
		return 0, fmt.Errorf("unknown grade method %q (supported: SERIAL_WINDOWS, SERIAL_LINUX)", s)
	}
}

func (m GradeMethod) String() string {
	switch m {
	case GradeMethodSerialWindows:
		return "SERIAL_WINDOWS"
	case GradeMethodSerialLinux:
		return "SERIAL_LINUX"
	default:
		return fmt.Sprintf("GradeMethod(%d)", int(m))
	}
}
