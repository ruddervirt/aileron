package ws

import (
	"strings"
	"testing"
)

const helloStr = "hello"

func TestDecodeChunkedOutput_Valid(t *testing.T) {
	encoded := crockfordEncoding.EncodeToString([]byte(helloStr))
	// Split encoded string into two chunks
	mid := len(encoded) / 2
	input := "some noise\nOUT:0:" + encoded[:mid] + "\nmore noise\nOUT:1:" + encoded[mid:] + "\ntrailing"

	result, err := decodeChunkedOutput(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(result) != helloStr {
		t.Errorf("got %q, want %q", string(result), helloStr)
	}
}

func TestDecodeChunkedOutput_SingleChunk(t *testing.T) {
	// "hi" in Crockford base32: DHGQ
	// Actually let's use the encoder to be sure
	encoded := crockfordEncoding.EncodeToString([]byte("test data"))
	input := "OUT:0:" + encoded + "\n"

	result, err := decodeChunkedOutput(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(result) != "test data" {
		t.Errorf("got %q, want %q", string(result), "test data")
	}
}

func TestDecodeChunkedOutput_NoChunks(t *testing.T) {
	_, err := decodeChunkedOutput("no chunk data here")
	if err == nil {
		t.Fatal("expected error for no chunks")
	}
}

func TestDecodeChunkedOutput_EmptyInput(t *testing.T) {
	_, err := decodeChunkedOutput("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestDecodeChunkedOutput_MissingChunk(t *testing.T) {
	// Chunks 0 and 2 but missing 1
	encoded := crockfordEncoding.EncodeToString([]byte("ab"))
	input := "OUT:0:" + encoded + "\nOUT:2:" + encoded

	_, err := decodeChunkedOutput(input)
	if err == nil {
		t.Fatal("expected error for missing chunk")
	}
}

func TestDecodeChunkedOutput_DuplicateChunk(t *testing.T) {
	encoded := crockfordEncoding.EncodeToString([]byte("ab"))
	input := "OUT:0:" + encoded + "\nOUT:0:" + encoded

	_, err := decodeChunkedOutput(input)
	if err == nil {
		t.Fatal("expected error for duplicate chunk")
	}
}

// TestStripANSI_DoesNotGlueAcrossRemovedEscapeSequence covers the actual
// root cause of a real grading incident: a cursor-repositioning CSI
// sequence sitting directly between the last OUT: chunk of a response and
// the terminal's returning shell prompt, with no \r\n in between (this is
// how a real SAC/EMS console emits it - the terminal repaints in place
// rather than scrolling). Deleting the escape sequence outright glues the
// chunk payload's last character onto the prompt's first character
// (both draw from the same alphanumeric alphabet), which outChunkRe's
// greedy match then silently absorbs into the chunk - corrupting the
// decoded payload by a few bits.
func TestStripANSI_DoesNotGlueAcrossRemovedEscapeSequence(t *testing.T) {
	in := "OUT:0:ABCDEFGH\x1b[18;1HC:\\Windows\\System32>"
	got := stripANSI(in)
	if strings.Contains(got, "ABCDEFGHC") {
		t.Fatalf("stripANSI glued the prompt's leading character onto the chunk payload: %q", got)
	}
}

// TestDecodeChunkedOutput_SurvivesTrailingEscapeSequence reproduces the
// exact shape of the incident transcript end-to-end: a chunk immediately
// followed by a cursor-jump CSI sequence and the return prompt, with no
// separating \r\n.
func TestDecodeChunkedOutput_SurvivesTrailingEscapeSequence(t *testing.T) {
	const want = "hello world"
	encoded := crockfordEncoding.EncodeToString([]byte(want))
	raw := "OUT:0:" + encoded + "\x1b[18;1HC:\\Windows\\System32>"

	result, err := decodeChunkedOutput(stripANSI(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != want {
		t.Errorf("got %q, want %q", string(result), want)
	}
}

func TestLimitString(t *testing.T) {
	if got := limitString(helloStr, 10); got != helloStr {
		t.Errorf("got %q", got)
	}
	if got := limitString("hello world", 5); got != "hello...[truncated]" {
		t.Errorf("got %q", got)
	}
	if got := limitString(helloStr, 0); got != helloStr {
		t.Errorf("got %q for max=0", got)
	}
}
