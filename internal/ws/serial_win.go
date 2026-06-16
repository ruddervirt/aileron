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

// RunCommandsWithRudderGrade executes commands using the Windows serial console grader
//
//nolint:gocyclo // pre-existing complexity; serial console state machine
func RunCommandsWithRudderGrade(wsConn *websocket.Conn, username, password, domain string, commands []string) ([]CommandResult, error) {
	if username == "" {
		username = "skills"
	}
	if password == "" {
		password = "skills"
	}
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

	sacPrompt := regexp.MustCompile(`SAC>`)
	channelRe := regexp.MustCompile(`(?i)Channel:\s+(Cmd\d+)\r?\n`)
	unableCmdRe := regexp.MustCompile(`Unable to launch a Command Prompt`)
	cmdAvailableRe := regexp.MustCompile(`EVENT: The CMD command is now available`)
	anyKeyRe := regexp.MustCompile(`(?i)any other key to view this channel`)
	channelSwitchPromptRe := regexp.MustCompile(`(?is)(Use any other key to view this channel|Error:\s+Could not find a channel with that name)`)
	usernameRe := regexp.MustCompile(`Username`)
	domainRe := regexp.MustCompile(`Domain`)
	passwordRe := regexp.MustCompile(`Password`)
	windowsPromptRe := regexp.MustCompile(`(?i)[A-Z]:\\Windows\\system32>\s*`)
	authFailRe := regexp.MustCompile(`(?i)unable to authenticate`)

	//nolint:unparam // first result is always nil so callers can `return failErr(err)` directly
	failErr := func(err error) ([]CommandResult, error) {
		if err == nil {
			err = fmt.Errorf("unknown error")
		}
		slog.Error("serial console interaction failed", "error", err)
		scroll.dumpToLogger()
		return nil, err
	}

	failf := func(format string, args ...any) ([]CommandResult, error) {
		return failErr(fmt.Errorf(format, args...))
	}

	for {
		if err := console.clearAndEnter(); err != nil {
			return failf("failed to clear console: %v", err)
		}

		if _, _, err := console.expectRegex(sacPrompt, 2*time.Second); err == nil {
			break
		} else if !isTimeoutError(err) {
			return failErr(err)
		}

		for range 4 {
			if err := console.sendSlow("\r"); err != nil {
				return failf("failed to send newline: %v", err)
			}
		}

		if err := console.sendSlow("exit\r"); err != nil {
			return failf("failed to send exit command: %v", err)
		}

		sleep(500 * time.Millisecond)

		if err := console.sendSlow("\x1b\t0"); err != nil {
			return failf("failed to send channel selection shortcut: %v", err)
		}
	}

	if err := console.clearAndEnter(); err != nil {
		return failf("failed to clear console before cmd: %v", err)
	}

	if _, _, err := console.expectRegex(sacPrompt, 5*time.Second); err != nil {
		slog.Warn("failed to get SAC prompt after clear", "error", err)
	}

	// Clean up orphaned command channels
	console.resetBuffer()
	if err := console.sendSlow("ch\r"); err != nil {
		return failf("failed to list channels for cleanup: %v", err)
	}

	initChOutput, _, err := console.expectRegex(sacPrompt, 5*time.Second)
	if err != nil {
		slog.Warn("failed to list channels for cleanup", "error", err)
	} else if channelListContainsCmd(initChOutput) {
		slog.Info("cleaning up orphaned command channels")
		for i := 1; i <= 9; i++ {
			if err := console.sendSlow(fmt.Sprintf("ch -ci %d\r", i)); err != nil {
				slog.Warn("failed to clean channel", "index", i, "error", err)
				break
			}
			sleep(500 * time.Millisecond)
			_, _, _ = console.expectRegex(sacPrompt, 3*time.Second)
		}
	}
	console.resetBuffer()

	errChannelUnavailable := fmt.Errorf("command channel unavailable")

	captureChannel := func() (string, error) {
		for range 6 {
			segment, matches, err := console.expectRegex(channelRe, 20*time.Second)
			if err == nil {
				candidate := strings.TrimSpace(matches[1])
				lowerSegment := strings.ToLower(segment)
				if strings.Contains(lowerSegment, "a channel has been closed") && !strings.Contains(lowerSegment, "a new channel has been created") {
					continue
				}
				return candidate, nil
			}

			if !isTimeoutError(err) {
				return "", err
			}

			if _, _, err := console.expectRegex(unableCmdRe, 2*time.Second); err != nil {
				if isTimeoutError(err) {
					return "", fmt.Errorf("cmd failed to produce a command channel")
				}
				return "", err
			}

			if _, _, err := console.expectRegex(cmdAvailableRe, 30*time.Second); err != nil {
				return "", err
			}
		}
		return "", fmt.Errorf("failed to identify command channel after retries")
	}

	switchChannel := func(name string) error {
		if err := console.send(fmt.Sprintf("ch -sn %s\r", name)); err != nil {
			return fmt.Errorf("failed to switch to channel %s: %w", name, err)
		}

		_, matches, err := console.expectRegex(channelSwitchPromptRe, 15*time.Second)
		if err != nil {
			return err
		}

		if strings.Contains(strings.ToLower(matches[1]), "error") {
			return errChannelUnavailable
		}

		return console.send("\r")
	}

	const maxChannelAttempts = 3
	const maxSwitchRetries = 2
	var (
		channelName string
		switchErr   error
	)

	for attempt := range maxChannelAttempts {
		if attempt > 0 {
			if err := console.clearAndEnter(); err != nil {
				return failf("failed to clear console before retry: %v", err)
			}
			console.resetBuffer()
		}

		if err := console.send("cmd\r"); err != nil {
			return failf("failed to send cmd: %v", err)
		}

		name, err := captureChannel()
		if err != nil {
			return failErr(err)
		}
		channelName = name
		sleep(500 * time.Millisecond)

		var switched bool
		for range maxSwitchRetries {
			if err := switchChannel(channelName); err != nil {
				switchErr = err
				if err == errChannelUnavailable {
					sleep(time.Second)
					continue
				}
				return failErr(err)
			}
			switched = true
			break
		}

		if switched {
			switchErr = nil
			break
		}
	}

	if switchErr != nil {
		return failErr(switchErr)
	}

	expectSend := func(r *regexp.Regexp, answer string, timeout time.Duration) error {
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		if _, _, err := console.expectRegex(r, timeout); err != nil {
			return err
		}
		return console.send(answer + "\r")
	}

	if err := expectSend(usernameRe, username, 10*time.Second); err != nil {
		return failErr(err)
	}
	if err := expectSend(domainRe, domain, 5*time.Second); err != nil {
		return failErr(err)
	}
	if err := expectSend(passwordRe, password, 10*time.Second); err != nil {
		return failErr(err)
	}

	if err := console.clearAndEnter(); err != nil {
		return failf("failed to clear console before waiting for prompt: %v", err)
	}

	if err := expectSend(windowsPromptRe, "", 45*time.Second); err != nil {
		snapshot := console.snapshotForError()
		if authFailRe.MatchString(snapshot) {
			return failf("authentication failed: the username or password is incorrect (user: %q)", username)
		}
		return failErr(err)
	}

	sleep(10 * time.Second)

	if err := console.sendSlow("\r"); err != nil {
		return failf("failed to send newline after prompt wait: %v", err)
	}

	if err := expectSend(windowsPromptRe, "", 15*time.Second); err != nil {
		return failErr(err)
	}

	command := fmt.Sprintf("C:\\Windows\\system32\\ruddergrade.exe --alphanumeric %s", base64Request)
	slog.Info("dispatching ruddergrade command", "encodedLen", len(base64Request))
	if err := console.sendSlow(command); err != nil {
		return failf("failed to send ruddergrade command: %v", err)
	}
	if err := console.sendSlow("\r"); err != nil {
		return failf("failed to execute ruddergrade command: %v", err)
	}

	// Collect output
	var (
		outputBuilder       strings.Builder
		segments            int
		sawChunkPayload     bool
		outputWaitWindow    = 4 * time.Minute
		maxPromptSegments   = 20
		promptCheckInterval = 10 * time.Second
		extensionOnProgress = 30 * time.Second
		lastChunkCount      = 0
		lastActivityLen     = 0
		maxExtensions       = 20
		extensionsUsed      = 0
	)
	deadline := time.Now().Add(outputWaitWindow)

	// extendOn applies a deadline extension when EITHER a new OUT chunk
	// appeared OR the console buffer is still growing. The latter handles
	// ruddergrade.exe cold-loading the AD module repeatedly (one cold load
	// per command), where 4 minutes of "no chunks yet" doesn't mean the
	// session is hung — bytes are still arriving. We cap total extensions
	// so a genuinely hung session still fails in bounded time.
	extendOn := func(fullStripped string, bufLen int) bool {
		if extensionsUsed >= maxExtensions {
			return false
		}
		currentChunkCount := len(outChunkRe.FindAllString(fullStripped, -1))
		chunkProgress := currentChunkCount > lastChunkCount
		byteProgress := bufLen > lastActivityLen
		if !chunkProgress && !byteProgress {
			return false
		}
		lastChunkCount = currentChunkCount
		lastActivityLen = bufLen
		// Only ever move the deadline forward. The first expectRegex timeout
		// always sees byte progress (the command echo arriving after dispatch),
		// and re-arming the deadline to now+30s here would truncate the
		// 4-minute initial window to ~40s — failing any grade whose
		// ruddergrade run is silent for longer than that (e.g. SMB hashing).
		if nd := time.Now().Add(extensionOnProgress); nd.After(deadline) {
			deadline = nd
			extensionsUsed++
		}
		return true
	}

	for segments < maxPromptSegments {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			console.mu.Lock()
			bufSnapshot := console.buf.String()
			console.mu.Unlock()
			fullOutput := stripANSI(outputBuilder.String() + bufSnapshot)
			if extendOn(fullOutput, outputBuilder.Len()+len(bufSnapshot)) {
				continue
			}
			break
		}

		checkTimeout := minDuration(remaining, promptCheckInterval)
		segment, _, err := console.expectRegex(windowsPromptRe, checkTimeout)
		if err != nil {
			if isTimeoutError(err) {
				console.mu.Lock()
				bufSnapshot := console.buf.String()
				console.mu.Unlock()
				fullOutput := stripANSI(outputBuilder.String() + bufSnapshot)
				extendOn(fullOutput, outputBuilder.Len()+len(bufSnapshot))
				continue
			}
			return failErr(err)
		}
		outputBuilder.WriteString(segment)

		// Only count a segment if its stripped form contains real content
		// between prompts; pure screen redraws (e.g. PowerShell module loader
		// progress bar) repaint the prompt without emitting new output.
		strippedSegment := stripANSI(segment)
		nonPromptContent := strings.TrimSpace(windowsPromptRe.ReplaceAllString(strippedSegment, ""))
		if nonPromptContent != "" {
			segments++
		}

		if outChunkRe.MatchString(strippedSegment) {
			sawChunkPayload = true
			break
		}
	}

	console.mu.Lock()
	remainingBuf := console.buf.String()
	console.buf.Reset()
	console.mu.Unlock()
	if remainingBuf != "" {
		outputBuilder.WriteString(remainingBuf)
	}

	output := outputBuilder.String()

	strippedOutput := stripANSI(output)
	if !sawChunkPayload && !outChunkRe.MatchString(strippedOutput) {
		return failf("ruddergrade output did not include chunk payload before timeout")
	}

	jsonResponse, err := decodeChunkedOutput(strippedOutput)
	if err != nil {
		return failf("failed to decode ruddergrade output: %v", err)
	}

	// Cleanup: exit channel and return to SAC
	sleep(2 * time.Second)
	console.mu.Lock()
	console.buf.Reset()
	console.mu.Unlock()

	_ = console.send("exit\r\r")
	if _, _, err := console.expectRegex(anyKeyRe, 15*time.Second); err == nil {
		_ = console.send("\x1b\t0")
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
