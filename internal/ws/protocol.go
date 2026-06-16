package ws

import (
	"encoding/base32"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	debugSnippetLen   = 512
)

var (
	crockfordEncoding = base32.NewEncoding(crockfordAlphabet).WithPadding(base32.NoPadding)
	outChunkRe        = regexp.MustCompile(`OUT:(\d+):([A-Z0-9]+)`)
	// ansiCSIRe matches CSI sequences (ESC [ ... final-byte), e.g. cursor
	// positioning and erase-in-line. ansiOSCRe matches OSC sequences
	// (ESC ] ... BEL), e.g. window-title sets emitted by cmd.exe.
	ansiCSIRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	ansiOSCRe = regexp.MustCompile(`\x1b\][^\x07]*\x07`)
)

// stripANSI removes ANSI control sequences and stray NULs from terminal
// output so regex matching reflects what a human would see rather than the
// raw byte stream. Useful for prompt/chunk detection when the shell is
// repainting the screen (e.g. PowerShell progress bars).
func stripANSI(s string) string {
	s = ansiOSCRe.ReplaceAllString(s, "")
	s = ansiCSIRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\x00", "")
	return s
}

func decodeChunkedOutput(output string) ([]byte, error) {
	matches := outChunkRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no chunk lines present")
	}

	type chunk struct {
		id      int
		payload string
	}

	chunks := make([]chunk, 0, len(matches))
	for _, match := range matches {
		id, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("invalid chunk index %q", match[1])
		}
		chunks = append(chunks, chunk{id: id, payload: match[2]})
	}

	sort.Slice(chunks, func(i, j int) bool {
		return chunks[i].id < chunks[j].id
	})

	for i := 1; i < len(chunks); i++ {
		if chunks[i].id == chunks[i-1].id {
			return nil, fmt.Errorf("duplicate chunk index %d", chunks[i].id)
		}
		if chunks[i].id != chunks[i-1].id+1 {
			return nil, fmt.Errorf("missing chunk between %d and %d", chunks[i-1].id, chunks[i].id)
		}
	}

	if chunks[0].id != 0 {
		return nil, fmt.Errorf("first chunk index must be 0, got %d", chunks[0].id)
	}

	var payloadBuilder strings.Builder
	for _, c := range chunks {
		payloadBuilder.WriteString(c.payload)
	}

	decoded, err := crockfordEncoding.DecodeString(payloadBuilder.String())
	if err != nil {
		return nil, fmt.Errorf("invalid base32 payload: %w", err)
	}

	return decoded, nil
}

func limitString(input string, max int) string {
	if max <= 0 {
		return input
	}
	runes := []rune(input)
	if len(runes) <= max {
		return input
	}
	return string(runes[:max]) + "...[truncated]"
}
