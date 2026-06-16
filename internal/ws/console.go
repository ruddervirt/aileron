package ws

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxBufferSize    = 1024 * 1024
	bufferTrimSize   = 100 * 1024
	errorSnapshotLen = 512
	slowPause        = 250 * time.Millisecond
)

var (
	ansiCSIRegex       = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")
	ansiOSCRegex       = regexp.MustCompile("\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)")
	danglingCSIRegex   = regexp.MustCompile(`\[[0-9?;]{1,6}[0-9?;]*[@-~]`)
	danglingOSCTitleRe = regexp.MustCompile(`\]0;[^\r\n]*`)
	whitespaceRegex    = regexp.MustCompile(`\s+`)
	controlCharRegex   = regexp.MustCompile(`[[:cntrl:]]`)
)

var sleep = time.Sleep

type consoleScrollback struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *consoleScrollback) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *consoleScrollback) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

func (s *consoleScrollback) dumpToLogger() {
	data := s.snapshot()
	if len(data) == 0 {
		slog.Error("---- console scrollback: <empty> ----")
		return
	}
	slog.Error("---- console scrollback ----")
	slog.Error(string(data))
	slog.Error("---- end console scrollback ----")
}

type wsConsole struct {
	conn   *websocket.Conn
	scroll *consoleScrollback

	mu      sync.Mutex
	buf     bytes.Buffer
	updates chan struct{}
	done    chan struct{}

	errMu   sync.Mutex
	readErr error
}

func newWSConsole(conn *websocket.Conn, scroll *consoleScrollback) *wsConsole {
	c := &wsConsole{
		conn:    conn,
		scroll:  scroll,
		updates: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *wsConsole) Close() {
	select {
	case <-c.done:
		return
	default:
		close(c.done)
		c.notifyUpdate()
	}
}

func (c *wsConsole) send(data string) error {
	if err := c.conn.WriteMessage(websocket.BinaryMessage, []byte(data)); err != nil {
		slog.Debug("binary write failed, retrying as text", "error", err)
		if err := c.conn.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
			return fmt.Errorf("failed to write to console: %w", err)
		}
	}
	return nil
}

func (c *wsConsole) sendSlow(data string) error {
	sleep(slowPause)
	if err := c.send(data); err != nil {
		return err
	}
	sleep(slowPause)
	return nil
}

func (c *wsConsole) clearAndEnter() error {
	if err := c.send("\x03"); err != nil {
		return err
	}
	sleep(100 * time.Millisecond)
	return c.send("\r")
}

func (c *wsConsole) resetBuffer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

func (c *wsConsole) expectRegex(re *regexp.Regexp, timeout time.Duration) (string, []string, error) {
	deadline := time.Now().Add(timeout)

	for {
		c.mu.Lock()
		data := c.buf.String()
		if idx := re.FindStringSubmatchIndex(data); idx != nil {
			consumed := data[:idx[1]]
			submatches := re.FindStringSubmatch(consumed)
			c.buf.Next(idx[1])
			c.mu.Unlock()
			return consumed, submatches, nil
		}
		c.mu.Unlock()

		if err := c.getReadErr(); err != nil {
			return "", nil, fmt.Errorf("console read error while waiting for %q: %w", re.String(), err)
		}

		if time.Now().After(deadline) {
			return "", nil, &expectTimeoutError{
				pattern:  re.String(),
				snapshot: c.snapshotForError(),
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = 10 * time.Millisecond
		}

		select {
		case <-time.After(minDuration(remaining, 500*time.Millisecond)):
		case <-c.updates:
		case <-c.done:
			if err := c.getReadErr(); err != nil {
				return "", nil, err
			}
			return "", nil, fmt.Errorf("console closed while waiting for %q", re.String())
		}
	}
}

func (c *wsConsole) readLoop() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.setReadErr(err)
			c.notifyUpdate()
			return
		}

		if len(data) == 0 {
			continue
		}

		_, _ = c.scroll.Write(data)

		// SAC and cmd.exe interleave NUL bytes around prompts and command
		// echoes (e.g. "\x00Username:", "C:\\Windows\\\x00system32>"). Those
		// NULs are not visible to a human but split regex patterns and
		// cause expectRegex to time out even when the prompt is present.
		// Keep the raw bytes in the scrollback for debugging, but strip
		// NULs from the matching buffer.
		filtered := data
		if bytes.IndexByte(data, 0) >= 0 {
			filtered = bytes.ReplaceAll(data, []byte{0}, nil)
		}

		c.mu.Lock()
		c.buf.Write(filtered)
		if c.buf.Len() > maxBufferSize {
			buffer := c.buf.Bytes()
			c.buf.Reset()
			c.buf.Write(buffer[len(buffer)-bufferTrimSize:])
		}
		c.mu.Unlock()

		c.notifyUpdate()
	}
}

func (c *wsConsole) notifyUpdate() {
	select {
	case c.updates <- struct{}{}:
	default:
	}
}

func (c *wsConsole) setReadErr(err error) {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	c.readErr = err
}

func (c *wsConsole) getReadErr() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.readErr
}

func (c *wsConsole) snapshotForError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	data := c.buf.String()
	if len(data) > errorSnapshotLen {
		data = data[len(data)-errorSnapshotLen:]
	}
	return cleanControlChars(data)
}

type expectTimeoutError struct {
	pattern  string
	snapshot string
}

func (e *expectTimeoutError) Error() string {
	if e.snapshot == "" {
		return fmt.Sprintf("timed out waiting for pattern %q", e.pattern)
	}
	return fmt.Sprintf("timed out waiting for pattern %q (snapshot: %s)", e.pattern, e.snapshot)
}

func (e *expectTimeoutError) Timeout() bool { return true }

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) {
		return te.Timeout()
	}
	return false
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}

func cleanControlChars(input string) string {
	cleaned := ansiOSCRegex.ReplaceAllString(input, "")
	cleaned = ansiCSIRegex.ReplaceAllString(cleaned, "")
	cleaned = danglingOSCTitleRe.ReplaceAllString(cleaned, "")
	cleaned = danglingCSIRegex.ReplaceAllString(cleaned, "")
	cleaned = whitespaceRegex.ReplaceAllString(cleaned, " ")
	cleaned = controlCharRegex.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func channelListContainsCmd(output string) bool {
	idx := strings.LastIndex(output, "Channel List")
	if idx == -1 {
		return strings.Contains(output, "Cmd")
	}
	section := output[idx:]
	if promptIdx := strings.Index(section, "SAC>"); promptIdx != -1 {
		section = section[:promptIdx]
	}
	return strings.Contains(section, "Cmd")
}
