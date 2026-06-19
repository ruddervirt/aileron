// Package guac implements the Guacamole protocol wire codec shared by the VNC
// gateway (internal/vncgateway), which speaks it to both clients and guacd, and
// the coordinator's minimal client (internal/build/guacclient). The format is
// LEN.VALUE,LEN.VALUE,...; where LEN is the element's length in UTF-8 code
// points (not bytes). See docs/vncgateway.md for the surrounding architecture.
package guac

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Instruction is one Guacamole protocol instruction.
type Instruction struct {
	Opcode string
	Args   []string
}

// Encode serializes an instruction to the Guacamole wire format:
// LEN.VALUE,LEN.VALUE,...; where LEN is the element's length in UTF-8 code
// points (not bytes).
func Encode(opcode string, args ...string) []byte {
	var b strings.Builder
	writeElem := func(s string) {
		b.WriteString(strconv.Itoa(utf8.RuneCountInString(s)))
		b.WriteByte('.')
		b.WriteString(s)
	}
	writeElem(opcode)
	for _, a := range args {
		b.WriteByte(',')
		writeElem(a)
	}
	b.WriteByte(';')
	return []byte(b.String())
}

// Decoder incrementally parses instructions from a byte stream. Instructions
// may span multiple Feed calls and one call may carry several instructions.
type Decoder struct {
	buf []byte
}

// Feed appends raw bytes received from the wire.
func (d *Decoder) Feed(data []byte) {
	d.buf = append(d.buf, data...)
}

// Next returns the next complete instruction, or nil if the buffered data
// does not yet contain one. A malformed stream returns an error.
func (d *Decoder) Next() (*Instruction, error) {
	ins, _, err := d.NextRaw()
	return ins, err
}

// NextRaw is like Next but also returns the exact wire bytes of the returned
// instruction (nil when no complete instruction is available yet). The
// returned slice aliases the decoder's internal buffer and is only valid until
// the next Feed call; the gateway relay forwards it immediately, before reading
// (and feeding) more bytes, so aliasing is safe there. Copy it if you retain it
// across a Feed.
func (d *Decoder) NextRaw() (*Instruction, []byte, error) {
	pos := 0
	var elems []string

	for {
		// Parse the decimal length prefix.
		i := pos
		for i < len(d.buf) && d.buf[i] >= '0' && d.buf[i] <= '9' {
			i++
		}
		if i >= len(d.buf) {
			return nil, nil, nil // incomplete
		}
		if i == pos {
			return nil, nil, fmt.Errorf("guac decode: expected length digit at offset %d, got %q", pos, d.buf[i])
		}
		if d.buf[i] != '.' {
			return nil, nil, fmt.Errorf("guac decode: expected '.' after length at offset %d, got %q", i, d.buf[i])
		}
		n, err := strconv.Atoi(string(d.buf[pos:i]))
		if err != nil {
			return nil, nil, fmt.Errorf("guac decode: bad length %q: %w", d.buf[pos:i], err)
		}

		// Consume n UTF-8 code points.
		j := i + 1
		for range n {
			if j >= len(d.buf) || !utf8.FullRune(d.buf[j:]) {
				return nil, nil, nil // incomplete
			}
			_, size := utf8.DecodeRune(d.buf[j:])
			j += size
		}
		if j >= len(d.buf) {
			return nil, nil, nil // terminator not yet received
		}
		elems = append(elems, string(d.buf[i+1:j]))

		switch d.buf[j] {
		case ',':
			pos = j + 1
		case ';':
			raw := d.buf[:j+1]
			d.buf = d.buf[j+1:]
			return &Instruction{Opcode: elems[0], Args: elems[1:]}, raw, nil
		default:
			return nil, nil, fmt.Errorf("guac decode: expected ',' or ';' at offset %d, got %q", j, d.buf[j])
		}
	}
}
