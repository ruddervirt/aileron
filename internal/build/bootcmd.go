package build

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// BootAction is a single action parsed from a boot command string.
type BootAction interface {
	bootAction()
}

// KeyAction sends a key press and/or release via VNC.
type KeyAction struct {
	Keysym uint32
	Down   bool // true = press only (no release)
	Up     bool // true = release only (no press)
}

func (KeyAction) bootAction() {}

// WaitAction pauses for a duration.
type WaitAction struct {
	Duration time.Duration
}

func (WaitAction) bootAction() {}

// X11 keysym lookup table for special keys.
var specialKeys = map[string]uint32{
	"enter":     0xff0d,
	"return":    0xff0d,
	"tab":       0xff09,
	"esc":       0xff1b,
	"escape":    0xff1b,
	"bs":        0xff08,
	"backspace": 0xff08,
	"del":       0xffff,
	"delete":    0xffff,
	"insert":    0xff63,
	"home":      0xff50,
	"end":       0xff57,
	"pageUp":    0xff55,
	"pageDown":  0xff56,
	"left":      0xff51,
	"up":        0xff52,
	"right":     0xff53,
	"down":      0xff54,
	"space":     0x0020,
	"spacebar":  0x0020,
	"f1":        0xffbe,
	"f2":        0xffbf,
	"f3":        0xffc0,
	"f4":        0xffc1,
	"f5":        0xffc2,
	"f6":        0xffc3,
	"f7":        0xffc4,
	"f8":        0xffc5,
	"f9":        0xffc6,
	"f10":       0xffc7,
	"f11":       0xffc8,
	"f12":       0xffc9,
}

// KeysymNames maps keysyms back to human-readable names for logging.
var KeysymNames = func() map[uint32]string {
	m := map[uint32]string{
		0xffe1: "shift",
		0xffe2: "rightShift",
		0xffe3: "ctrl",
		0xffe4: "rightCtrl",
		0xffe9: "alt",
		0xffea: "rightAlt",
	}
	for name, sym := range specialKeys {
		if _, exists := m[sym]; !exists {
			m[sym] = name
		}
	}
	return m
}()

// Modifier key mappings — press-only and release-only variants.
var modifierKeys = map[string]struct {
	Keysym uint32
	Down   bool // true = press-only, false = release-only
}{
	"leftCtrlOn":    {0xffe3, true},
	"leftCtrlOff":   {0xffe3, false},
	"leftAltOn":     {0xffe9, true},
	"leftAltOff":    {0xffe9, false},
	"leftShiftOn":   {0xffe1, true},
	"leftShiftOff":  {0xffe1, false},
	"rightCtrlOn":   {0xffe4, true},
	"rightCtrlOff":  {0xffe4, false},
	"rightAltOn":    {0xffea, true},
	"rightAltOff":   {0xffea, false},
	"rightShiftOn":  {0xffe2, true},
	"rightShiftOff": {0xffe2, false},
}

// waitRe matches <waitN> or <waitNs> where N is an integer number of seconds.
var waitRe = regexp.MustCompile(`^wait(\d+)s?$`)

// ResolveBootCommandTemplates replaces {{ .Key }} template variables in boot
// command strings. This is used to inject HTTP server addresses (HTTPIP, HTTPPort)
// into boot commands before parsing.
func ResolveBootCommandTemplates(commands []string, vars map[string]string) []string {
	result := make([]string, len(commands))
	for i, cmd := range commands {
		s := cmd
		for k, v := range vars {
			s = strings.ReplaceAll(s, "{{ ."+k+" }}", v)
			s = strings.ReplaceAll(s, "{{."+k+"}}", v)
		}
		result[i] = s
	}
	return result
}

// ParseBootCommands parses a list of Packer-style boot command strings into actions.
func ParseBootCommands(commands []string) ([]BootAction, error) {
	var actions []BootAction
	for _, cmd := range commands {
		parsed, err := parseBootLine(cmd)
		if err != nil {
			return nil, err
		}
		actions = append(actions, parsed...)
	}
	return actions, nil
}

func parseBootLine(line string) ([]BootAction, error) {
	var actions []BootAction
	i := 0
	for i < len(line) {
		if line[i] == '<' {
			// Find matching >.
			end := strings.IndexByte(line[i:], '>')
			if end == -1 {
				// No closing bracket — treat as literal.
				actions = appendChar(actions, rune(line[i]))
				i++
				continue
			}
			tag := line[i+1 : i+end]
			i += end + 1

			a, err := parseTag(tag)
			if err != nil {
				return nil, err
			}
			actions = append(actions, a...)
		} else {
			r := rune(line[i])
			// Handle multi-byte UTF-8.
			if r >= 0x80 {
				remaining := line[i:]
				for _, ch := range remaining {
					r = ch
					break
				}
				i += len(string(r))
			} else {
				i++
			}
			actions = appendChar(actions, r)
		}
	}
	return actions, nil
}

func parseTag(tag string) ([]BootAction, error) {
	lower := strings.ToLower(tag)

	// Wait.
	if lower == "wait" {
		return []BootAction{WaitAction{Duration: time.Second}}, nil
	}
	if m := waitRe.FindStringSubmatch(lower); m != nil {
		n, _ := strconv.Atoi(m[1])
		return []BootAction{WaitAction{Duration: time.Duration(n) * time.Second}}, nil
	}

	// Modifier keys (case-sensitive lookup on original tag).
	if mod, ok := modifierKeys[tag]; ok {
		return []BootAction{KeyAction{Keysym: mod.Keysym, Down: mod.Down, Up: !mod.Down}}, nil
	}

	// Special keys.
	if keysym, ok := specialKeys[lower]; ok {
		return []BootAction{KeyAction{Keysym: keysym}}, nil
	}

	return nil, fmt.Errorf("unknown boot command tag: <%s>", tag)
}

// shiftedPunct lists the US-keyboard punctuation characters whose keysyms
// require Shift to be held to produce the right scancode in QEMU's keymap.
// Uppercase letters are handled separately via unicode.IsUpper.
var shiftedPunct = map[rune]bool{
	'!': true, '@': true, '#': true, '$': true, '%': true,
	'^': true, '&': true, '*': true, '(': true, ')': true,
	'_': true, '+': true, '{': true, '}': true, '|': true,
	':': true, '"': true, '<': true, '>': true, '?': true,
	'~': true,
}

// appendChar adds key actions for a character. X11 keysyms for printable ASCII
// equal their Unicode codepoints — 'A' is 0x41, '>' is 0x3e, 'g' is 0x67.
// For shifted characters we send Shift down + the displayed keysym + Shift up.
// Sending the *unshifted* keysym (e.g. 'g' or '.') with Shift held does not
// reliably produce 'G' or '>' in QEMU — its keymap looks up the scancode for
// the keysym you sent, so 'g' types 'g' even with Shift held.
func appendChar(actions []BootAction, r rune) []BootAction {
	if unicode.IsUpper(r) || shiftedPunct[r] {
		return append(actions,
			KeyAction{Keysym: 0xffe1, Down: true}, // Shift press
			KeyAction{Keysym: uint32(r)},          // Key press+release with the displayed keysym
			KeyAction{Keysym: 0xffe1, Up: true},   // Shift release
		)
	}
	return append(actions, KeyAction{Keysym: uint32(r)})
}
