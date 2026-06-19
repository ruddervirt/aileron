package guac

import (
	"reflect"
	"testing"
)

func TestEncode(t *testing.T) {
	tests := []struct {
		opcode string
		args   []string
		want   string
	}{
		{"key", []string{"65307", "1"}, "3.key,5.65307,1.1;"},
		{"key", []string{"65307", "0"}, "3.key,5.65307,1.0;"},
		{"sync", []string{"12345"}, "4.sync,5.12345;"},
		{"nop", nil, "3.nop;"},
		// Length counts code points, not bytes: é is 2 bytes, 1 code point.
		{"clipboard", []string{"é"}, "9.clipboard,1.é;"},
	}
	for _, tt := range tests {
		got := string(Encode(tt.opcode, tt.args...))
		if got != tt.want {
			t.Errorf("Encode(%q, %v) = %q, want %q", tt.opcode, tt.args, got, tt.want)
		}
	}
}

func TestDecoderRoundTrip(t *testing.T) {
	var d Decoder
	d.Feed(Encode("key", "65307", "1"))
	d.Feed(Encode("sync", "98765"))

	ins, err := d.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := &Instruction{Opcode: "key", Args: []string{"65307", "1"}}
	if !reflect.DeepEqual(ins, want) {
		t.Errorf("got %+v, want %+v", ins, want)
	}

	ins, err = d.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want = &Instruction{Opcode: "sync", Args: []string{"98765"}}
	if !reflect.DeepEqual(ins, want) {
		t.Errorf("got %+v, want %+v", ins, want)
	}

	// Buffer drained.
	ins, err = d.Next()
	if err != nil || ins != nil {
		t.Errorf("expected empty decoder, got %+v err %v", ins, err)
	}
}

func TestDecoderSplitFeeds(t *testing.T) {
	full := "3.key,5.65307,1.1;"
	for split := 1; split < len(full); split++ {
		var d Decoder
		d.Feed([]byte(full[:split]))
		ins, err := d.Next()
		if err != nil {
			t.Fatalf("split %d: unexpected error: %v", split, err)
		}
		if ins != nil {
			t.Fatalf("split %d: got instruction from partial data: %+v", split, ins)
		}
		d.Feed([]byte(full[split:]))
		ins, err = d.Next()
		if err != nil {
			t.Fatalf("split %d: Next: %v", split, err)
		}
		want := &Instruction{Opcode: "key", Args: []string{"65307", "1"}}
		if !reflect.DeepEqual(ins, want) {
			t.Errorf("split %d: got %+v, want %+v", split, ins, want)
		}
	}
}

func TestDecoderMultiBytePartialRune(t *testing.T) {
	full := Encode("clipboard", "héllo")
	// Split in the middle of the 2-byte é.
	mid := 0
	for i, b := range full {
		if b == 0xc3 { // first byte of é
			mid = i + 1
			break
		}
	}
	if mid == 0 {
		t.Fatal("did not find multi-byte rune in encoded form")
	}

	var d Decoder
	d.Feed(full[:mid])
	if ins, err := d.Next(); err != nil || ins != nil {
		t.Fatalf("partial rune: got %+v err %v", ins, err)
	}
	d.Feed(full[mid:])
	ins, err := d.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := &Instruction{Opcode: "clipboard", Args: []string{"héllo"}}
	if !reflect.DeepEqual(ins, want) {
		t.Errorf("got %+v, want %+v", ins, want)
	}
}

func TestDecoderMalformed(t *testing.T) {
	for _, raw := range []string{
		"x.key;",       // no length digits
		"3,key;",       // missing dot
		"3.key!5.a;",   // bad separator
		"3.key,2.ab!;", // bad terminator
	} {
		var d Decoder
		d.Feed([]byte(raw))
		if _, err := d.Next(); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}

// TestNextRawReturnsExactWireBytes verifies the raw slice the relay forwards is
// byte-identical to what was fed, across an instruction boundary within one
// feed (the relay drains all complete instructions before the next Feed).
func TestNextRawReturnsExactWireBytes(t *testing.T) {
	first := Encode("size", "0", "1024", "768")
	second := Encode("img", "1", "14", "0", "0", "0")
	var d Decoder
	d.Feed(append(append([]byte{}, first...), second...))

	ins, raw, err := d.NextRaw()
	if err != nil {
		t.Fatalf("NextRaw: %v", err)
	}
	if ins.Opcode != "size" {
		t.Fatalf("opcode = %q, want size", ins.Opcode)
	}
	if string(raw) != string(first) {
		t.Errorf("raw = %q, want %q", raw, first)
	}

	ins, raw, err = d.NextRaw()
	if err != nil {
		t.Fatalf("NextRaw: %v", err)
	}
	if ins.Opcode != "img" {
		t.Fatalf("opcode = %q, want img", ins.Opcode)
	}
	if string(raw) != string(second) {
		t.Errorf("raw = %q, want %q", raw, second)
	}

	ins, raw, err = d.NextRaw()
	if err != nil || ins != nil || raw != nil {
		t.Errorf("expected drained decoder, got ins=%+v raw=%q err=%v", ins, raw, err)
	}
}
