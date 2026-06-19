package vncgateway

import (
	"regexp"
	"strings"
)

// dns1123 mirrors what Kubernetes enforces for resource names. namespace and
// vmi land verbatim in VMI lookups and the upstream KubeVirt URL path;
// accepting anything else could smuggle `..`, `?`, `#`, or other control
// characters. Parity with internal/vncbridge/bridge.go's validName and the
// former vncgateway/lib/path.js.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validName(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	return dns1123.MatchString(s)
}

// parseTwoSegments extracts and validates "{a}/{b}" after the given prefix,
// tolerating a trailing slash. Returns the two segments and true, or false if
// the path does not match exactly two valid DNS-1123 segments.
func parseTwoSegments(pathname, prefix string) (string, string, bool) {
	if !strings.HasPrefix(pathname, prefix) {
		return "", "", false
	}
	rest := strings.TrimRight(pathname[len(prefix):], "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}
