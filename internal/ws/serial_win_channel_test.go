package ws

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeSACChannelServer upgrades to a websocket and counts every "cmd\r" the
// client sends. On the first "cmd\r" it replies with SAC's real
// "Unable to launch a Command Prompt ... has not yet registered" error,
// waits eventDelay, then emits the "EVENT: The CMD command is now
// available" line - exactly the sequence captured from the production
// incident. On any subsequent "cmd\r" (the resend under test) it replies
// with a channel line, simulating the service actually being ready.
func fakeSACChannelServer(t *testing.T, eventDelay time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	var cmdCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if !strings.Contains(string(msg), "cmd\r") {
				continue
			}

			n := atomic.AddInt32(&cmdCount, 1)
			if n == 1 {
				_ = conn.WriteMessage(websocket.BinaryMessage, []byte(
					"Error: Unable to launch a Command Prompt.  The service responsible for launching\r\n"+
						"       Command Prompt channels has not yet registered.\r\nSAC>\r\n"))
				time.Sleep(eventDelay)
				_ = conn.WriteMessage(websocket.BinaryMessage, []byte(
					"EVENT: The CMD command is now available.\r\nSAC>\r\n"))
				continue
			}

			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("Channel: Cmd0001\r\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &cmdCount
}

// This is a regression test for a production incident: a Windows grading
// run failed with "cmd failed to produce a command channel" even though the
// console scrollback showed SAC explicitly reporting
// "EVENT: The CMD command is now available" right after rejecting the
// original "cmd" invocation. The bug was that captureCommandChannel (née
// the captureChannel closure) never resent "cmd" after seeing that event -
// SAC does not retry the launch on its own - so it just waited again for a
// channel that nothing would ever create.
func TestCaptureCommandChannel_ResendsCmdAfterServiceBecomesAvailable(t *testing.T) {
	channelRe := regexp.MustCompile(`(?i)Channel:\s+(Cmd\d+)\r?\n`)
	unableCmdRe := regexp.MustCompile(`Unable to launch a Command Prompt`)
	cmdAvailableRe := regexp.MustCompile(`EVENT: The CMD command is now available`)

	srv, cmdCount := fakeSACChannelServer(t, 30*time.Millisecond)
	console := dialTestConsole(t, srv)
	defer console.Close()

	if err := console.send("cmd\r"); err != nil {
		t.Fatalf("send initial cmd: %v", err)
	}

	name, err := captureCommandChannel(console, channelRe, unableCmdRe, cmdAvailableRe,
		200*time.Millisecond, 200*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("expected success once cmd is resent after the service becomes available, got error: %v", err)
	}
	if name != "Cmd0001" {
		t.Fatalf("got channel %q, want Cmd0001", name)
	}
	if got := atomic.LoadInt32(cmdCount); got != 2 {
		t.Fatalf("expected exactly 2 cmd invocations (original + resend), got %d", got)
	}
}

// Verifies the pre-existing fast-fail path still works: if SAC never even
// reports the "unable to launch" error (nothing resembling a channel or a
// registration problem ever shows up), captureCommandChannel should give up
// promptly rather than exhausting all 6 retries.
func TestCaptureCommandChannel_FailsFastWithNoUnableMessage(t *testing.T) {
	channelRe := regexp.MustCompile(`(?i)Channel:\s+(Cmd\d+)\r?\n`)
	unableCmdRe := regexp.MustCompile(`Unable to launch a Command Prompt`)
	cmdAvailableRe := regexp.MustCompile(`EVENT: The CMD command is now available`)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	console := dialTestConsole(t, srv)
	defer console.Close()

	if err := console.send("cmd\r"); err != nil {
		t.Fatalf("send initial cmd: %v", err)
	}

	start := time.Now()
	_, err := captureCommandChannel(console, channelRe, unableCmdRe, cmdAvailableRe,
		100*time.Millisecond, 100*time.Millisecond, time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected an error, got success")
	}
	if want := "cmd failed to produce a command channel"; err.Error() != want {
		t.Fatalf("got error %q, want %q", err.Error(), want)
	}
	// Should bail after the first channelTimeout+unableCmdCheckTimeout, not
	// after 6 retries worth of waiting.
	if elapsed > time.Second {
		t.Fatalf("expected a fast failure, took %v", elapsed)
	}
}

// Sanity check: an immediate channel match still succeeds without needing
// any of the "unable to launch" recovery path.
func TestCaptureCommandChannel_SucceedsOnFirstMatch(t *testing.T) {
	channelRe := regexp.MustCompile(`(?i)Channel:\s+(Cmd\d+)\r?\n`)
	unableCmdRe := regexp.MustCompile(`Unable to launch a Command Prompt`)
	cmdAvailableRe := regexp.MustCompile(`EVENT: The CMD command is now available`)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("Channel: Cmd0002\r\n"))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	console := dialTestConsole(t, srv)
	defer console.Close()

	if err := console.send("cmd\r"); err != nil {
		t.Fatalf("send initial cmd: %v", err)
	}

	name, err := captureCommandChannel(console, channelRe, unableCmdRe, cmdAvailableRe,
		time.Second, 100*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "Cmd0002" {
		t.Fatalf("got channel %q, want Cmd0002", name)
	}
}
