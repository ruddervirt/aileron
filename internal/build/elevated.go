package build

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// SanitizeLogLine strips characters that should never appear in a provisioner
// log line — NULs and BELs that leak in from PS 5.1 UTF-16 log capture or
// terminal control sequences. The trailing CR from CRLF endings is also
// dropped here so callers don't have to remember to do it themselves.
func SanitizeLogLine(s string) string {
	if !strings.ContainsAny(s, "\x00\a\r") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 0, '\a':
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), "\r")
}

// IsBlankLogLine reports whether a sanitized log line should be suppressed
// because it carries no visible content.
func IsBlankLogLine(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// elevatedStatusMarker is the first token on the final stdout line of the
// wrapper's Poll phase. Lines above it are user-script output.
const elevatedStatusMarker = "__AILERON_STATUS__"

const (
	elevatedStateRunning   = "running"
	elevatedStateCompleted = "completed"
)

const (
	// elevatedExitNotElevated is the sentinel the runner writes when its
	// scheduled-task token fails the Administrator role check: the SSH
	// account is not a local admin, or UAC token filtering applied. The
	// target script is never run in that case.
	elevatedExitNotElevated = 252

	// schedNeverRan is Windows Task Scheduler's SCHED_S_TASK_HAS_NOT_RUN
	// (0x41303). Completing with this LastTaskResult means the task was
	// registered but never actually launched — typically a stored-credential
	// or batch-logon-rights problem.
	schedNeverRan = 267011
)

// describeElevatedExit maps sentinel exit codes from the elevated wrapper to
// descriptive errors so callers surface an actionable message instead of a
// bare numeric code. Returns nil for codes the caller should interpret
// itself (including ordinary script failures).
func describeElevatedExit(code int) error {
	switch code {
	case elevatedExitNotElevated:
		return fmt.Errorf(
			"elevated task token was not elevated: the SSH account is not a local Administrator, " +
				"or UAC token filtering prevented elevation (script was not run)")
	case schedNeverRan:
		return fmt.Errorf(
			"scheduled task never launched (Task Scheduler 0x41303 SCHED_S_TASK_HAS_NOT_RUN); " +
				"check stored-credential registration and the account's 'Log on as a batch job' rights")
	}
	return nil
}

// elevatedPollInterval is the cadence at which RunElevated polls the wrapper
// for new output and task state. Small enough that progress feels live,
// large enough to keep SSH dial overhead off the hot path.
const elevatedPollInterval = 2 * time.Second

// elevatedSSHRetryInterval is the cadence at which pollWithRetry re-attempts
// after an SSH error. Mirrors sshRetryInterval in cmd/coordinator so the
// elevated path retries with the same cadence as waitForSSH does during
// boot. Upper bound is governed by the build context.
const elevatedSSHRetryInterval = 15 * time.Second

// RunElevated launches scriptPath on the Windows VM as a detached, fully-
// elevated scheduled task and streams its output via onLine. It returns the
// task's exit code on completion.
//
// The wrapper is driven through short, independent SSH commands (Start,
// repeated Polls, Cleanup) so any session loss — e.g. the target script
// tearing down its own NIC IP mid-run — is recovered by the next Poll
// retry rather than wedging the build. The scheduled task itself is
// detached from the SSH session, so it keeps running across reconnects.
//
// wrapperPath is the on-VM path to ElevatedRunnerScript, which the caller
// must have uploaded once during coordinator startup.
//
// env is injected into the elevated child as $env: assignments (base64
// transport through the wrapper's -EnvB64 parameter). Values must not
// contain newlines.
//
// A non-zero exit code is returned without wrapping in an error so callers
// like Windows Update can branch on specific sentinel codes (e.g. 101).
// The exception is the wrapper's own sentinels (not-elevated, never-launched):
// those return a descriptive non-nil error alongside the code so no caller
// mistakes them for a script-level exit.
func RunElevated(
	ctx context.Context,
	comm *SSHCommunicator,
	wrapperPath, scriptPath string,
	env map[string]string,
	onLine func(string),
) (int, error) {
	if err := uploadElevationCreds(ctx, comm); err != nil {
		return -1, fmt.Errorf("uploading elevation credentials: %w", err)
	}

	startCmd := fmt.Sprintf(
		`powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%s" -Phase Start -Script "%s"`,
		wrapperPath, scriptPath,
	)
	if envB64 := encodeElevatedEnv(env); envB64 != "" {
		// Base64's alphabet ([A-Za-z0-9+/=]) is safe as a bare argument
		// through the cmd.exe -> powershell.exe boundary — no quoting needed.
		startCmd += " -EnvB64 " + envB64
	}
	out, err := comm.RunCommand(ctx, startCmd, nil)
	// Emit Start output even on error: Register/launch diagnostics (including
	// the wrapper's WARNING lines and any thrown PowerShell error text) must
	// reach the provisioner log, not just the wrapped error string.
	emitLines(out, onLine)
	if err != nil {
		return -1, fmt.Errorf("starting elevated task: %w\noutput: %s", err, out)
	}

	// Cleanup is best-effort: failures don't fail the build because the
	// next Start phase unregisters and overwrites the task anyway.
	defer runElevatedCleanup(ctx, comm, wrapperPath)

	var offset int64
	for {
		select {
		case <-time.After(elevatedPollInterval):
		case <-ctx.Done():
			return -1, ctx.Err()
		}

		pollCmd := fmt.Sprintf(
			`powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%s" -Phase Poll -Offset %d`,
			wrapperPath, offset,
		)
		pollOut, err := pollWithRetry(ctx, comm, pollCmd)
		if err != nil {
			return -1, fmt.Errorf("polling elevated task: %w", err)
		}

		state, newOffset, exitCode := splitPollOutput(pollOut, offset, onLine)
		offset = newOffset
		if state == elevatedStateCompleted {
			if exitCode == nil {
				return -1, fmt.Errorf("elevated task completed without exit code")
			}
			if err := describeElevatedExit(*exitCode); err != nil {
				return *exitCode, err
			}
			return *exitCode, nil
		}
	}
}

// encodeElevatedEnv serializes env as base64-encoded NAME=VALUE lines for the
// wrapper's -EnvB64 parameter. Keys are sorted so the generated command line
// is deterministic. Returns "" for an empty map.
func encodeElevatedEnv(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	return base64.StdEncoding.EncodeToString([]byte(b.String()))
}

// uploadElevationCreds writes the SSH user's username and password to the
// path the wrapper's Start phase reads on entry. The wrapper deletes the
// file as its first action, so credentials only sit on disk for the brief
// window between this SFTP write and Start's Remove-Item call.
func uploadElevationCreds(ctx context.Context, comm *SSHCommunicator) error {
	content := comm.Username + "\n" + comm.Password + "\n"
	return comm.UploadFile(ctx, []byte(content), ElevatedCredsPath)
}

func runElevatedCleanup(ctx context.Context, comm *SSHCommunicator, wrapperPath string) {
	cmd := fmt.Sprintf(
		`powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%s" -Phase Cleanup`,
		wrapperPath,
	)
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _ = comm.RunCommand(cleanupCtx, cmd, nil)
}

// elevatedPollPerCallTimeout bounds each Poll's RunCommand independently of
// the build context. Without this, a half-dead SSH session wedges the read forever.
// golang.org/x/crypto/ssh has no keepalive of its own, so this deadline is the only
// thing that unblocks the goroutine and lets pollWithRetry actually retry.
const elevatedPollPerCallTimeout = 30 * time.Second

// pollWithRetry runs Poll, retrying on SSH errors with the same cadence as
// waitForSSH. Transient SSH loss — the whole reason this wrapper is phased —
// must not abort the build; the build context (typically the per-build
// timeout) is the only upper bound.
func pollWithRetry(ctx context.Context, comm *SSHCommunicator, cmd string) (string, error) {
	for {
		callCtx, cancel := context.WithTimeout(ctx, elevatedPollPerCallTimeout)
		out, err := comm.RunCommand(callCtx, cmd, nil)
		cancel()
		if err == nil {
			return out, nil
		}
		select {
		case <-time.After(elevatedSSHRetryInterval):
		case <-ctx.Done():
			return "", fmt.Errorf("polling cancelled while retrying SSH: %w", ctx.Err())
		}
	}
}

// splitPollOutput pulls the trailing status marker off the Poll response,
// emits each preceding line via onLine, and parses the marker. priorOffset
// is returned unchanged when the marker is missing (truncated/garbled Poll
// response) so the next Poll resumes from where it left off instead of
// replaying the log from byte zero.
func splitPollOutput(out string, priorOffset int64, onLine func(string)) (state string, offset int64, exitCode *int) {
	lines := strings.Split(strings.TrimRight(out, "\r\n"), "\n")
	statusLine := ""
	if n := len(lines); n > 0 {
		last := strings.TrimRight(lines[n-1], "\r")
		if strings.HasPrefix(last, elevatedStatusMarker) {
			statusLine = last
			lines = lines[:n-1]
		}
	}
	for _, l := range lines {
		l = SanitizeLogLine(l)
		if IsBlankLogLine(l) {
			continue
		}
		if onLine != nil {
			onLine(l)
		}
	}
	if statusLine == "" {
		return elevatedStateRunning, priorOffset, nil
	}
	return parseElevatedStatus(statusLine)
}

func parseElevatedStatus(line string) (state string, offset int64, exitCode *int) {
	fields := strings.Fields(line)
	if len(fields) < 1 || fields[0] != elevatedStatusMarker {
		return elevatedStateRunning, 0, nil
	}
	for _, f := range fields[1:] {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "state":
			state = kv[1]
		case "offset":
			if n, err := strconv.ParseInt(kv[1], 10, 64); err == nil {
				offset = n
			}
		case "exit":
			if n, err := strconv.Atoi(kv[1]); err == nil {
				exitCode = &n
			}
		}
	}
	return state, offset, exitCode
}

func emitLines(out string, onLine func(string)) {
	if onLine == nil {
		return
	}
	for l := range strings.SplitSeq(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		onLine(l)
	}
}
