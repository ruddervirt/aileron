package ws

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakePromptServer upgrades to a websocket, counts every message the client
// sends (the wake newlines waitForPromptWithRegex sends between attempts),
// and, if sendAfter is non-negative, writes promptText to the client after
// that delay. A negative sendAfter means a matching prompt is never sent -
// the client is left to time out.
func fakePromptServer(t *testing.T, promptText string, sendAfter time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	var writes int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				atomic.AddInt32(&writes, 1)
			}
		}()

		if sendAfter >= 0 {
			select {
			case <-time.After(sendAfter):
				_ = conn.WriteMessage(websocket.BinaryMessage, []byte(promptText))
			case <-done:
				return
			}
		}

		<-done
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

func dialTestConsole(t *testing.T, srv *httptest.Server) *wsConsole {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsTestURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return newWSConsole(conn, &consoleScrollback{})
}

// This is a regression test for a production incident: two Linux grading
// runs against a healthy VM failed with "timed out waiting for pattern"
// after exactly 15s, even though the command budget was 90s. The bug was
// that waitForPromptWithRegex's retry loop was bounded by a fixed attempt
// count instead of the deadline, so its real ceiling was
// attempts*wakeDelay - 15s - regardless of the much larger totalTimeout
// the caller asked for.
func TestWaitForPromptWithRegex_HonorsFullTotalTimeout(t *testing.T) {
	const prompt = "\r\nroot@debian:~# "
	wakeDelay := 40 * time.Millisecond
	totalTimeout := 300 * time.Millisecond
	oldBuggyCeiling := 3 * wakeDelay // the old fixed-attempts cap
	sendAfter := oldBuggyCeiling + 80*time.Millisecond

	srv, _ := fakePromptServer(t, prompt, sendAfter)
	console := dialTestConsole(t, srv)
	defer console.Close()

	start := time.Now()
	_, _, err := waitForPromptWithRegex(console, linuxPromptRe, totalTimeout, wakeDelay)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected success once the prompt arrives at %v (well within the %v budget), got error: %v", sendAfter, totalTimeout, err)
	}
	if elapsed < sendAfter {
		t.Fatalf("returned before the prompt was even sent: elapsed %v < sendAfter %v", elapsed, sendAfter)
	}
}

// Verifies every retry uses the wakeDelay the caller passed in, not a
// hardcoded constant - a second bug in the same function that made
// waitForLinuxShell's 1s initial wake cadence silently jump to 5s after its
// first retry.
func TestWaitForPromptWithRegex_WakesAtProvidedInterval(t *testing.T) {
	wakeDelay := 30 * time.Millisecond
	totalTimeout := 300 * time.Millisecond // ~10 wake intervals at this cadence

	srv, writes := fakePromptServer(t, "", -1) // never produces a matching prompt
	console := dialTestConsole(t, srv)
	defer console.Close()

	if _, _, err := waitForPromptWithRegex(console, linuxPromptRe, totalTimeout, wakeDelay); err == nil {
		t.Fatalf("expected a timeout error, got success")
	}

	got := atomic.LoadInt32(writes)
	// The old fixed-attempts (3) loop would send at most 2 wake newlines for
	// any totalTimeout/wakeDelay combination.
	if got <= 2 {
		t.Fatalf("expected more than 2 wake newlines for a %v budget at %v cadence, got %d", totalTimeout, wakeDelay, got)
	}
}

func TestWaitForPromptWithRegex_TimesOutWhenNoMatch(t *testing.T) {
	totalTimeout := 150 * time.Millisecond
	wakeDelay := 30 * time.Millisecond

	srv, _ := fakePromptServer(t, "", -1)
	console := dialTestConsole(t, srv)
	defer console.Close()

	start := time.Now()
	_, _, err := waitForPromptWithRegex(console, linuxPromptRe, totalTimeout, wakeDelay)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a timeout error")
	}
	if elapsed < totalTimeout {
		t.Fatalf("returned before totalTimeout elapsed: %v < %v", elapsed, totalTimeout)
	}
}

func TestWaitForPromptWithRegex_PropagatesNonTimeoutErrorImmediately(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close() // hang up before the client ever reads anything
	}))
	defer srv.Close()

	console := dialTestConsole(t, srv)
	defer console.Close()

	start := time.Now()
	_, _, err := waitForPromptWithRegex(console, linuxPromptRe, 5*time.Second, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected an error from the closed console")
	}
	if elapsed > time.Second {
		t.Fatalf("expected fast failure on a closed connection, took %v", elapsed)
	}
}
