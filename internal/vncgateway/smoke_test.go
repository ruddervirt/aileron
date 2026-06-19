package vncgateway

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// TestGuacdHandshakeSmoke validates the handshake byte-shape against a REAL
// guacd (the fake guacd in the other tests can't catch a missing/misordered
// handshake instruction that real guacd 1.5.5 requires — see docs/vncgateway.md).
// It is skipped unless VNCGATEWAY_SMOKE_GUACD_ADDR points at a running
// guacamole/guacd:1.5.5 (e.g. `docker run --rm -p 4822:4822 guacamole/guacd:1.5.5`,
// then VNCGATEWAY_SMOKE_GUACD_ADDR=127.0.0.1:4822).
//
// VNCGATEWAY_SMOKE_VNC ("host:port", default 127.0.0.1:5900) is the VNC target
// guacd is told to dial; it need not be reachable — guacd sends `ready` after
// accepting our handshake and before confirming the VNC leg, so reaching
// `ready` proves the handshake syntax is accepted. If a real VNC server is
// provided, the test also waits for a display instruction.
func TestGuacdHandshakeSmoke(t *testing.T) {
	addr := os.Getenv("VNCGATEWAY_SMOKE_GUACD_ADDR")
	if addr == "" {
		t.Skip("set VNCGATEWAY_SMOKE_GUACD_ADDR to run the real-guacd smoke test")
	}
	vnc := os.Getenv("VNCGATEWAY_SMOKE_VNC")
	if vnc == "" {
		vnc = "127.0.0.1:5900"
	}
	host, port, err := net.SplitHostPort(vnc)
	if err != nil {
		t.Fatalf("invalid VNCGATEWAY_SMOKE_VNC %q: %v", vnc, err)
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial guacd %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	gd := &guacdConn{conn: conn, rbuf: make([]byte, 32*1024)}
	settings := map[string]string{
		"hostname":    host,
		"port":        port,
		"color-depth": "24",
		"encodings":   "zrle copyrect",
	}
	if err := gd.handshake("vnc", settings); err != nil {
		t.Fatalf("handshake against real guacd failed: %v", err)
	}
	if !strings.HasPrefix(gd.connID, "$") {
		t.Fatalf("real guacd did not return a connection id; got %q", gd.connID)
	}
	t.Logf("real guacd accepted handshake, connection id %s", gd.connID)

	// Best-effort: if a real VNC backend is reachable, a display instruction
	// should follow. Don't fail the handshake check if it isn't.
	for range 50 {
		ins, _, err := gd.readInstruction()
		if err != nil {
			return
		}
		switch ins.Opcode {
		case opSize, "img", "blob", "rect":
			t.Logf("received display instruction %q — full path works", ins.Opcode)
			return
		case opError, opDisconnect:
			t.Logf("guacd %s (expected if no VNC backend at %s): %v", ins.Opcode, vnc, ins.Args)
			return
		}
	}
}
