package clone

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxNameLength is the Kubernetes name length ceiling generated clone
// resource names must stay under.
const maxNameLength = 63

// truncateName shortens name to fit within maxNameLength. Simply slicing a
// name that overflows the limit risks landing mid-word on a trailing '-'
// (invalid: DNS-1123 names must end alphanumeric) and risks collisions
// between distinct inputs that share a long common prefix. Instead, cut
// content off and replace it with a short hash of the full input, so the
// result is both a valid name and still unique per input.
func truncateName(name string) string {
	if len(name) <= maxNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:4])
	prefix := strings.TrimRight(name[:maxNameLength-len(suffix)], "-")
	return prefix + suffix
}
