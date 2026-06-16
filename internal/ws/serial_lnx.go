package ws

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	linuxPromptMaxAttempts    = 3
	linuxInitialWakeDelay     = 1 * time.Second
	linuxPostCommandWakeDelay = 5 * time.Second
)

var linuxPromptRe = regexp.MustCompile(`(?s)(?:^|[\r\n])[^\r\n]*[#$](?:\x1b\[[0-9;]*[A-Za-z]|[ \t])*(?:\r?\n)?\z`)
var linuxLoginRe = regexp.MustCompile(`(?i)login\s*:\s*$`)
var linuxPasswordRe = regexp.MustCompile(`(?i)password\s*:\s*$`)
var linuxPromptOrLoginRe = regexp.MustCompile(linuxPromptRe.String() + `|` + linuxLoginRe.String())

// RunCommandsWithRudderGradeLinux executes commands using the Linux serial console grader
func RunCommandsWithRudderGradeLinux(wsConn *websocket.Conn, username, password, _ string, commands []string) ([]CommandResult, error) {
	request := struct {
		Commands []string `json:"commands"`
	}{
		Commands: commands,
	}

	requestJSON, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal command request: %w", err)
	}

	base64Request := base64.StdEncoding.EncodeToString(requestJSON)

	scroll := &consoleScrollback{}
	console := newWSConsole(wsConn, scroll)
	defer console.Close()

	//nolint:unparam // first result is always nil so callers can `return failErr(err)` directly
	failErr := func(err error) ([]CommandResult, error) {
		if err == nil {
			err = fmt.Errorf("unknown error")
		}
		slog.Error("linux serial interaction failed", "error", err)
		scroll.dumpToLogger()
		return nil, err
	}

	failf := func(format string, args ...any) ([]CommandResult, error) {
		return failErr(fmt.Errorf(format, args...))
	}

	if err := waitForLinuxShell(console, username, password); err != nil {
		return failErr(err)
	}

	command := fmt.Sprintf("/bin/ruddergrade --alphanumeric %s", base64Request)
	slog.Info("dispatching linux ruddergrade command", "encodedLen", len(base64Request), "commands", len(commands))

	if err := console.send(command + "\n"); err != nil {
		return failf("failed to send ruddergrade command: %v", err)
	}

	outputWithPrompt, promptMatches, err := waitForLinuxPrompt(console, 90*time.Second, linuxPostCommandWakeDelay, linuxPromptMaxAttempts)
	if err != nil {
		return failErr(err)
	}

	commandBody := strings.TrimSuffix(outputWithPrompt, promptMatches[0])
	commandBody = strings.TrimLeft(commandBody, "\r\n ")
	commandBody = strings.TrimPrefix(commandBody, command)
	commandBody = strings.TrimLeft(commandBody, "\r\n ")
	commandBody = cleanControlChars(commandBody)

	jsonResponse, err := decodeChunkedOutput(commandBody)
	if err != nil {
		return failf("failed to decode ruddergrade output: %v", err)
	}

	if len(jsonResponse) == 0 {
		return failf("no output captured from ruddergrade")
	}

	var response struct {
		Results []CommandResult `json:"results"`
		Error   string          `json:"error,omitempty"`
	}

	if err := json.Unmarshal(jsonResponse, &response); err != nil {
		return failf("failed to parse response JSON: %v", err)
	}

	if response.Error != "" {
		return failf("executor reported error: %s", response.Error)
	}

	return response.Results, nil
}

// waitForLinuxShell gets the console to a shell prompt, logging in first if
// the VM is sitting at a login screen.
func waitForLinuxShell(console *wsConsole, username, password string) error {
	segment, _, err := waitForPromptWithRegex(console, linuxPromptOrLoginRe, 10*time.Second, linuxInitialWakeDelay, linuxPromptMaxAttempts)
	if err != nil {
		return err
	}

	if !linuxLoginRe.MatchString(segment) {
		return nil
	}

	slog.Info("detected login prompt, authenticating", "user", username)

	if err := console.send(username + "\n"); err != nil {
		return fmt.Errorf("failed to send username: %w", err)
	}

	if _, _, err := console.expectRegex(linuxPasswordRe, 10*time.Second); err != nil {
		return fmt.Errorf("timed out waiting for password prompt: %w", err)
	}

	if err := console.send(password + "\n"); err != nil {
		return fmt.Errorf("failed to send password: %w", err)
	}

	// Wait for the MOTD to finish, then reset and send a newline so the
	// next prompt match is a real shell prompt, not a false positive from
	// MOTD content (e.g. the "#1" in a kernel version string).
	sleep(3 * time.Second)
	console.resetBuffer()

	if err := console.send("\n"); err != nil {
		return fmt.Errorf("failed to send post-login newline: %w", err)
	}

	if _, _, err := waitForLinuxPrompt(console, 15*time.Second, linuxPostCommandWakeDelay, linuxPromptMaxAttempts); err != nil {
		return fmt.Errorf("failed to get shell prompt after login: %w", err)
	}

	return nil
}

func waitForLinuxPrompt(console *wsConsole, totalTimeout, wakeDelay time.Duration, maxAttempts int) (string, []string, error) {
	return waitForPromptWithRegex(console, linuxPromptRe, totalTimeout, wakeDelay, maxAttempts)
}

func waitForPromptWithRegex(console *wsConsole, re *regexp.Regexp, totalTimeout, wakeDelay time.Duration, maxAttempts int) (string, []string, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	deadline := time.Now().Add(totalTimeout)
	if wakeDelay <= 0 || wakeDelay > totalTimeout {
		wakeDelay = totalTimeout
	}

	var lastErr error
	currentDelay := wakeDelay

	for attempt := 0; attempt < maxAttempts; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		if currentDelay > remaining {
			currentDelay = remaining
		}

		segment, matches, err := console.expectRegex(re, currentDelay)
		if err == nil {
			return segment, matches, nil
		}

		if !isTimeoutError(err) {
			return "", nil, err
		}

		lastErr = err

		if attempt == maxAttempts-1 {
			break
		}

		if err := console.send("\n"); err != nil {
			return "", nil, fmt.Errorf("failed to send wake newline: %w", err)
		}

		rest := time.Until(deadline)
		if rest <= 0 {
			break
		}

		currentDelay = min(linuxPostCommandWakeDelay, rest)
	}

	if lastErr != nil {
		return "", nil, lastErr
	}

	return "", nil, fmt.Errorf("timed out waiting for linux prompt")
}
