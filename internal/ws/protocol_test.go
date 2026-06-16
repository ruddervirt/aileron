package ws

import (
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
