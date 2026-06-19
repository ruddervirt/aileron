package vncgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ruddervirt/aileron/internal/guac"
)

// --- fake guacd ---------------------------------------------------------

// fakeGuacd is a TCP server standing in for guacd. It performs the handshake
// the real daemon does (select -> args, connect -> ready/size/sync) and can
// later kill live connections by emitting an `error` instruction WITHOUT
// closing the socket, reproducing guacd's observed zombie-socket behavior.
type fakeGuacd struct {
	ln       net.Listener
	argNames []string // advertised after VERSION_1_1_0

	mu      sync.Mutex
	conns   []*fakeGuacdConn
	liveID  string // most recent CONNECT id; "" once killed (stale-join detection)
	nextNum int
}

type fakeGuacdConn struct {
	sock     net.Conn
	writeMu  sync.Mutex
	selector string
	joined   bool
	id       string
	dead     bool
	received []*guac.Instruction
}

func startFakeGuacd(t *testing.T, argNames ...string) *fakeGuacd {
	t.Helper()
	if len(argNames) == 0 {
		argNames = []string{"hostname", "port"}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake guacd listen: %v", err)
	}
	f := &fakeGuacd{ln: ln, argNames: argNames}
	go f.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeGuacd) addr() string { return f.ln.Addr().String() }

func (f *fakeGuacd) acceptLoop() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		conn := &fakeGuacdConn{sock: c}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		go f.serve(conn)
	}
}

func (f *fakeGuacd) serve(conn *fakeGuacdConn) {
	var dec guac.Decoder
	buf := make([]byte, 4096)
	for {
		n, err := conn.sock.Read(buf)
		if err != nil {
			return
		}
		dec.Feed(buf[:n])
		for {
			ins, derr := dec.Next()
			if derr != nil || ins == nil {
				break
			}
			f.handle(conn, ins)
		}
	}
}

func (f *fakeGuacd) handle(conn *fakeGuacdConn, ins *guac.Instruction) {
	f.mu.Lock()
	conn.received = append(conn.received, ins)
	f.mu.Unlock()

	switch ins.Opcode {
	case "select":
		f.mu.Lock()
		conn.selector = ins.Args[0]
		conn.joined = strings.HasPrefix(ins.Args[0], "$")
		f.mu.Unlock()
		args := append([]string{"VERSION_1_1_0"}, f.argNames...)
		conn.write(guac.Encode("args", args...))
	case "connect":
		f.mu.Lock()
		stale := conn.joined && conn.selector != f.liveID
		if stale {
			f.mu.Unlock()
			// "Connection does not exist" — error, socket left open.
			conn.write(guac.Encode(opError, "Connection does not exist", "519"))
			return
		}
		if !conn.joined {
			f.nextNum++
			f.liveID = fmt.Sprintf("$conn-%d", f.nextNum)
		}
		conn.id = f.liveID
		id := conn.id
		f.mu.Unlock()
		conn.write(guac.Encode("ready", id))
		conn.write(guac.Encode(opSize, "0", "800", "600"))
		conn.write(guac.Encode("sync", "1"))
	}
}

// killAll emits an error instruction on every live connection without closing
// it (guacd's observed behavior), and forgets the live id so a subsequent join
// to it would be treated as stale.
func (f *fakeGuacd) killAll() {
	f.mu.Lock()
	f.liveID = ""
	targets := make([]*fakeGuacdConn, 0, len(f.conns))
	for _, c := range f.conns {
		if c.id != "" && !c.dead {
			c.dead = true
			targets = append(targets, c)
		}
	}
	f.mu.Unlock()
	for _, c := range targets {
		c.write(guac.Encode(opError, "Aborted. See logs.", "515"))
	}
}

func (f *fakeGuacd) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

// openedCount is the number of connections that completed CONNECT (have an id).
func (f *fakeGuacd) openedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.conns {
		if c.id != "" {
			n++
		}
	}
	return n
}

// snap returns a locked snapshot of the i-th connection's observable state.
func (f *fakeGuacd) snap(i int) (selector, id string, joined bool, received []*guac.Instruction, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.conns) {
		return "", "", false, nil, false
	}
	c := f.conns[i]
	received = append([]*guac.Instruction(nil), c.received...)
	return c.selector, c.id, c.joined, received, true
}

// sawKeyOn reports whether the i-th connection received a key instruction with
// the given keysym (forwarded from a client).
func (f *fakeGuacd) sawKeyOn(i int, keysym string) bool {
	_, _, _, received, ok := f.snap(i)
	if !ok {
		return false
	}
	for _, ins := range received {
		if ins.Opcode == "key" && len(ins.Args) > 0 && ins.Args[0] == keysym {
			return true
		}
	}
	return false
}

func connectArgs(received []*guac.Instruction) []string {
	for _, ins := range received {
		if ins.Opcode == "connect" {
			return ins.Args
		}
	}
	return nil
}

func (c *fakeGuacdConn) write(b []byte) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.sock.Write(b)
}

// --- fake bridge --------------------------------------------------------

// fakeBridge implements the vncgateway.Bridge interface in-process.
type fakeBridge struct {
	port      int
	consoleUp atomic.Bool

	mu         sync.Mutex
	tunnelReqs []string // "ns/vmi" per EnsureTunnel call
}

func newFakeBridge(port int) *fakeBridge {
	b := &fakeBridge{port: port}
	b.consoleUp.Store(true)
	return b
}

func (b *fakeBridge) EnsureTunnel(_ context.Context, namespace, vmi string) (int, error) {
	b.mu.Lock()
	b.tunnelReqs = append(b.tunnelReqs, namespace+"/"+vmi)
	b.mu.Unlock()
	return b.port, nil
}

func (b *fakeBridge) Probe(_ context.Context, _, _ string) error {
	if b.consoleUp.Load() {
		return nil
	}
	return errors.New("console not up")
}

func (b *fakeBridge) tunnelCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.tunnelReqs)
}

// --- gateway harness ----------------------------------------------------

func testConfig(guacdAddr string) Config {
	return Config{
		GuacdAddr:       guacdAddr,
		ConsoleWait:     5 * time.Second,
		ProbeInterval:   50 * time.Millisecond,
		ConnectSettings: map[string]string{"color-depth": "24", "encodings": "zrle copyrect"},
		JoinSettings:    map[string]string{"read-only": "false"},
	}
}

// startGateway runs the gateway on an httptest server and returns it plus the
// HTTP base URL. Use wsURL(httpBase, path) for WebSocket dials.
func startGateway(t *testing.T, cfg Config, bridge Bridge) (*Gateway, string) {
	t.Helper()
	gw := New(cfg, bridge, testLogger(t))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return gw, srv.URL
}

func wsURL(httpBase, path string) string {
	return "ws" + strings.TrimPrefix(httpBase, "http") + path
}

// --- test websocket client ----------------------------------------------

type testClient struct {
	ws *websocket.Conn

	mu          sync.Mutex
	sizes       int
	blanks      int // cfill
	errs        int
	nops        int
	display     bool
	lastErrCode string

	closed atomic.Bool
}

func dialClient(t *testing.T, url string) *testClient {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	c := &testClient{ws: ws}
	go c.readLoop()
	t.Cleanup(func() { _ = ws.Close() })
	return c
}

func (c *testClient) readLoop() {
	defer c.closed.Store(true)
	var dec guac.Decoder
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		dec.Feed(data)
		for {
			ins, derr := dec.Next()
			if derr != nil {
				return
			}
			if ins == nil {
				break
			}
			c.note(ins)
		}
	}
}

func (c *testClient) note(ins *guac.Instruction) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ins.Opcode {
	case opSize:
		c.sizes++
		c.display = true
	case "img", "blob":
		c.display = true
	case "cfill":
		c.blanks++
	case opError:
		c.errs++
		if len(ins.Args) > 0 {
			c.lastErrCode = ins.Args[len(ins.Args)-1]
		}
	case "nop":
		c.nops++
	}
}

func (c *testClient) send(b []byte) error {
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

func (c *testClient) snapshot() (sizes, blanks, errs, nops int, display bool, errCode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sizes, c.blanks, c.errs, c.nops, c.display, c.lastErrCode
}

// --- helpers ------------------------------------------------------------
//

// testLogger returns a logger that discards output to keep test logs clean.
func testLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// waitFor polls until predicate is true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, what string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if predicate() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
