// SPDX-License-Identifier: GPL-3.0-only

package ws

import (
	"fmt"
	"regexp"
)

// Guest-OS "tells" that show up on a serial console before or during login.
// They let the grader turn an opaque prompt-timeout into an actionable "wrong
// OS" diagnostic when a VM is graded with the method for the other OS — almost
// always a stale or incorrect ruddervirt.io/os label upstream.
//
// Patterns are matched against a cleanControlChars-normalized snapshot, so they
// must not rely on line anchors (newlines are collapsed to spaces there).
var (
	// SAC> is the Windows Special Administration Console prompt — a dead
	// giveaway that the guest is Windows, not Linux.
	windowsConsoleTellRe = regexp.MustCompile(`SAC>|Special Administration Console|Microsoft Windows \[Version`)
	// A bare "login:" (the Windows SAC login channel prompts Username/Domain/
	// Password, never "login:") or a GNU/Linux banner marks a Linux guest.
	linuxConsoleTellRe = regexp.MustCompile(`(?i)\blogin:|GNU/Linux|\(none\) login`)
)

// detectGuestOS inspects a console snapshot for OS-specific signatures and
// returns "windows", "linux", or "" when it can't tell. Windows is checked
// first because SAC> is the highest-confidence signal.
func detectGuestOS(snapshot string) string {
	clean := cleanControlChars(snapshot)
	switch {
	case windowsConsoleTellRe.MatchString(clean):
		return "windows"
	case linuxConsoleTellRe.MatchString(clean):
		return "linux"
	default:
		return ""
	}
}

// annotateOSMismatch wraps err when the console snapshot shows a guest OS that
// disagrees with the grading method that was used. expectedOS is the OS the
// caller's method drives ("linux" or "windows"). It returns err unchanged when
// there's no failure or no clear mismatch, so it is safe to call on every
// failure path.
func annotateOSMismatch(err error, snapshot, expectedOS string) error {
	if err == nil {
		return err
	}
	detected := detectGuestOS(snapshot)
	if detected == "" || detected == expectedOS {
		return err
	}
	return fmt.Errorf(
		"guest looks like %s (detected from serial console) but was graded with the %s method — "+
			"check the VM's ruddervirt.io/os label: %w",
		detected, expectedOS, err)
}
