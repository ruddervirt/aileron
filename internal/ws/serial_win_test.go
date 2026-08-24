// SPDX-License-Identifier: GPL-3.0-only

package ws

import (
	_ "embed"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ruddergradeDispatchMarker is the fixed literal prefix
// RunCommandsWithRudderGrade uses for the command it types (see
// serial_win.go), and is also present in the captured transcript's echo
// of that same line - used both to find where the OUT: payload begins in
// the fixture and to detect, from the server side, when the client under
// test has actually dispatched its command.
const ruddergradeDispatchMarker = "ruddergrade.exe --alphanumeric"

// ruddergradeWindowsSession is the raw byte stream captured from a real,
// failed grading run against a Windows guest (SAC/EMS console). Decoding
// it offline (crockford-base32 + JSON) produces complete, valid grading
// results - the wire data was never actually lost - but the pre-fix
// collection loop in RunCommandsWithRudderGrade could still finalize on a
// stale/short capture of it. Replaying the exact incident bytes is the
// most faithful regression test for the fix.
//
//go:embed testdata/ruddergrade_windows_session.bin
var ruddergradeWindowsSession []byte

const wantRuddergradeStdout = `{"pf_wan_80_to_dmz":false,"dmz_ip_ok":true,"admin_login_ok":false,"dhcp_dmz_50":false,"http_no_redirect":false,"dmz_block_lan":false,"dhcp_lan_50":false,"webgui_port_8443":false,"webgui_https":true,"lan_to_dmz_ok":true}`

// fakeSerialConsoleServer upgrades to a WebSocket and replays a fixed,
// pre-recorded byte sequence back to the client in small, delayed
// fragments (simulating a slow/fragmented serial console), following the
// dripped-write pattern used elsewhere in the repo
// (internal/vncbridge/rfbstream_test.go, internal/vncgateway/fake_test.go).
//
// It's mostly an open-loop playback (doesn't react to most client input),
// with one exception: everything from splitAt onward (the OUT: chunk
// payload and cleanup) is withheld until the client actually sends its
// ruddergrade dispatch, detected via dispatchMarker in the client's
// outgoing bytes. Without this, replaying the whole fixed transcript at a
// constant fast pace would race ahead of the client's own real-time
// pacing sleeps (RunCommandsWithRudderGrade waits several real seconds
// around login/dispatch) and let it observe "future" output - such as the
// OUT: chunks - before it has even sent the command, which the real SAC
// session this was captured from could never produce.
func fakeSerialConsoleServer(t *testing.T, data []byte, splitAt int, dispatchMarker string, fragment int, delay time.Duration) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()

		// Drain client sends in the background so RunCommandsWithRudderGrade
		// never blocks writing to us. clientDone closes when the client
		// hangs up (our cue the session is over); dispatched closes the
		// moment we see the client's own command go out.
		clientDone := make(chan struct{})
		dispatched := make(chan struct{})
		var dispatchedOnce sync.Once
		go func() {
			defer close(clientDone)
			for {
				_, msg, err := ws.ReadMessage()
				if err != nil {
					return
				}
				if strings.Contains(string(msg), dispatchMarker) {
					dispatchedOnce.Do(func() { close(dispatched) })
				}
			}
		}()

		writeFragmented := func(chunk []byte) bool {
			for i := 0; i < len(chunk); i += fragment {
				end := min(i+fragment, len(chunk))
				if err := ws.WriteMessage(websocket.BinaryMessage, chunk[i:end]); err != nil {
					return false
				}
				time.Sleep(delay)
			}
			return true
		}

		if !writeFragmented(data[:splitAt]) {
			return
		}

		select {
		case <-dispatched:
		case <-clientDone:
			return
		case <-time.After(60 * time.Second):
			return
		}

		if !writeFragmented(data[splitAt:]) {
			return
		}

		select {
		case <-clientDone:
		case <-time.After(60 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsTestURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestRunCommandsWithRudderGrade_RealIncidentReplay(t *testing.T) {
	commands := []string{`powershell -NoProfile -ExecutionPolicy Bypass -File "C:\\ProgramData\\grader.ps1"`}

	splitAt := strings.Index(string(ruddergradeWindowsSession), "OUT:0:")
	if splitAt < 0 {
		t.Fatal("fixture is missing the OUT:0: chunk marker")
	}

	// Run several times with tight fragmentation to exercise the
	// settle-window fix under adversarial timing rather than relying on a
	// single lucky pass.
	for i := range 5 {
		srv := fakeSerialConsoleServer(t, ruddergradeWindowsSession, splitAt, ruddergradeDispatchMarker, 24, 8*time.Millisecond)
		conn, _, err := websocket.DefaultDialer.Dial(wsTestURL(srv), nil)
		if err != nil {
			t.Fatalf("run %d: dial: %v", i, err)
		}

		results, err := RunCommandsWithRudderGrade(conn, "skills", "skills", "", commands)
		_ = conn.Close()
		srv.Close()

		if err != nil {
			t.Fatalf("run %d: RunCommandsWithRudderGrade failed: %v", i, err)
		}
		if len(results) != 1 {
			t.Fatalf("run %d: expected 1 result, got %d", i, len(results))
		}
		if results[0].Stdout != wantRuddergradeStdout {
			t.Fatalf("run %d: stdout mismatch:\ngot:  %s\nwant: %s", i, results[0].Stdout, wantRuddergradeStdout)
		}
		if results[0].ExitCode != 0 {
			t.Fatalf("run %d: expected exit code 0, got %d", i, results[0].ExitCode)
		}
	}
}
