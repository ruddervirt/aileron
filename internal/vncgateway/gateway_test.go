package vncgateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ruddervirt/aileron/internal/guac"
)

// End-to-end: first client connects, second joins the same guacd connection,
// input flows, the debug page serves, and closing all clients drains the
// registry. Port of vncgateway/test/integration.test.js.
func TestGatewayEndToEnd(t *testing.T) {
	guacd := startFakeGuacd(t, "hostname", "port", "encodings", "color-depth", "read-only")
	bridge := newFakeBridge(59001)
	gw, httpBase := startGateway(t, testConfig(guacd.addr()), bridge)

	a := dialClient(t, wsURL(httpBase, "/internal/build-ns/bid-vm1"))
	waitFor(t, 5*time.Second, "first guacd connection", func() bool { return guacd.connCount() == 1 })
	waitFor(t, 5*time.Second, "primary handshake", func() bool {
		_, _, _, recv, ok := guacd.snap(0)
		return ok && connectArgs(recv) != nil
	})

	sel0, id0, _, recv0, _ := guacd.snap(0)
	if sel0 != "vnc" {
		t.Fatalf("first selector = %q, want vnc", sel0)
	}
	args := connectArgs(recv0)
	// connect values align with advertised args [VERSION, hostname, port, ...].
	if len(args) < 3 || args[1] != "127.0.0.1" || args[2] != "59001" {
		t.Fatalf("connect args = %v, want hostname 127.0.0.1 and port 59001", args)
	}
	if bridge.tunnelCount() != 1 {
		t.Fatalf("tunnel requests = %d, want 1", bridge.tunnelCount())
	}

	waitFor(t, 5*time.Second, "display to client A", func() bool {
		_, _, _, _, display, _ := a.snapshot()
		return display
	})

	// Second client joins transparently (one VNC connection per VMI).
	b := dialClient(t, wsURL(httpBase, "/internal/build-ns/bid-vm1"))
	waitFor(t, 5*time.Second, "second guacd connection", func() bool { return guacd.connCount() == 2 })
	waitFor(t, 5*time.Second, "join selector", func() bool {
		sel, _, _, _, ok := guacd.snap(1)
		return ok && sel != ""
	})
	sel1, _, joined1, _, _ := guacd.snap(1)
	if sel1 != id0 || !joined1 {
		t.Fatalf("second selector = %q, want JOIN on %q", sel1, id0)
	}
	if bridge.tunnelCount() != 1 {
		t.Fatalf("join must not request another tunnel; tunnels = %d", bridge.tunnelCount())
	}
	if gw.registry.Size() != 1 {
		t.Fatalf("registry size = %d, want 1", gw.registry.Size())
	}

	// Input from client A reaches guacd.
	if err := a.send(guac.Encode("key", "65293", "1")); err != nil {
		t.Fatalf("send key: %v", err)
	}
	waitFor(t, 5*time.Second, "key at guacd", func() bool { return guacd.sawKeyOn(0, "65293") })

	// Debug page (no auth on the core gateway).
	resp, err := http.Get(httpBase + "/internal/debug/build-ns/bid-vm1")
	if err != nil {
		t.Fatalf("debug GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("debug status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "guacamole-common") {
		t.Fatalf("debug page missing guacamole-common viewer")
	}

	// Closing all clients drains the registry.
	_ = a.ws.Close()
	_ = b.ws.Close()
	waitFor(t, 5*time.Second, "registry drained", func() bool { return gw.registry.Size() == 0 })
}

// Clients connecting before the console exists are held with nops and attach
// the moment it appears — no guacd churn, no client retry loop. Port of
// vncgateway/test/console-wait.test.js.
func TestConsoleHeldUntilUp(t *testing.T) {
	guacd := startFakeGuacd(t)
	bridge := newFakeBridge(59004)
	bridge.consoleUp.Store(false)
	cfg := testConfig(guacd.addr())
	cfg.ProbeInterval = 50 * time.Millisecond
	gw, httpBase := startGateway(t, cfg, bridge)

	c := dialClient(t, wsURL(httpBase, "/internal/ns/cw-vmi"))

	// Console down: held with nops, no guacd connections, no session.
	waitFor(t, 5*time.Second, "keepalive nops", func() bool {
		_, _, _, nops, _, _ := c.snapshot()
		return nops >= 3
	})
	if c.closed.Load() {
		t.Fatal("connection must stay open while waiting")
	}
	if guacd.connCount() != 0 {
		t.Fatalf("no guacd churn while down; conns = %d", guacd.connCount())
	}
	if gw.registry.Size() != 0 {
		t.Fatalf("registry size = %d, want 0", gw.registry.Size())
	}

	// Console appears: client attaches without reconnecting.
	bridge.consoleUp.Store(true)
	waitFor(t, 5*time.Second, "display after console up", func() bool {
		_, _, _, _, display, _ := c.snapshot()
		return display
	})
	if c.closed.Load() {
		t.Fatal("connection closed unexpectedly")
	}
	if guacd.connCount() != 1 {
		t.Fatalf("guacd conns = %d, want exactly 1", guacd.connCount())
	}
	if gw.registry.Size() != 1 {
		t.Fatalf("registry size = %d, want 1", gw.registry.Size())
	}
}

func TestConsoleWaitTimeout(t *testing.T) {
	guacd := startFakeGuacd(t)
	bridge := newFakeBridge(59004)
	bridge.consoleUp.Store(false)
	cfg := testConfig(guacd.addr())
	cfg.ProbeInterval = 50 * time.Millisecond
	cfg.ConsoleWait = 300 * time.Millisecond
	_, httpBase := startGateway(t, cfg, bridge)

	c := dialClient(t, wsURL(httpBase, "/internal/ns/cw-timeout"))
	waitFor(t, 5*time.Second, "timeout close", func() bool { return c.closed.Load() })
	_, _, errs, _, _, code := c.snapshot()
	if errs == 0 || code != "519" {
		t.Fatalf("expected a clean 519 error, got errs=%d code=%q", errs, code)
	}
	if guacd.connCount() != 0 {
		t.Fatalf("guacd conns = %d, want 0", guacd.connCount())
	}
}

// A static screen produces no guacd output; the session must survive silence
// (the Go relay has no inactivity timer). Port of quiet-screen.test.js,
// shortened — there is no 10s stock kill to outlive.
func TestQuietScreenSurvives(t *testing.T) {
	guacd := startFakeGuacd(t)
	bridge := newFakeBridge(59002)
	gw, httpBase := startGateway(t, testConfig(guacd.addr()), bridge)

	c := dialClient(t, wsURL(httpBase, "/internal/ns/quiet-vmi"))
	waitFor(t, 5*time.Second, "attached", func() bool { return gw.registry.Size() == 1 })

	time.Sleep(2 * time.Second) // silence

	if c.closed.Load() {
		t.Fatal("connection must survive a quiet screen")
	}
	if gw.registry.Size() != 1 {
		t.Fatalf("registry size = %d, want 1", gw.registry.Size())
	}
}

// A reattach client's websocket survives guacd death: the gateway swallows the
// terminal error, blanks the canvas, and re-attaches the SAME socket to a fresh
// session. Port of reattach.test.js.
func TestReattachSurvivesGuacdDeath(t *testing.T) {
	guacd := startFakeGuacd(t)
	bridge := newFakeBridge(59005)
	cfg := testConfig(guacd.addr())
	cfg.ProbeInterval = 50 * time.Millisecond
	gw, httpBase := startGateway(t, cfg, bridge)

	c := dialClient(t, wsURL(httpBase, "/internal/bid-ns/bid-server?reattach=1"))
	waitFor(t, 8*time.Second, "first attach display", func() bool {
		sizes, _, _, _, _, _ := c.snapshot()
		return sizes >= 1
	})
	if gw.registry.Size() != 1 {
		t.Fatalf("registry size = %d, want 1", gw.registry.Size())
	}

	guacd.killAll()
	waitFor(t, 8*time.Second, "re-attach display on same socket", func() bool {
		sizes, _, _, _, _, _ := c.snapshot()
		return sizes >= 2
	})
	if c.closed.Load() {
		t.Fatal("browser websocket must never close")
	}
	waitFor(t, 8*time.Second, "fresh guacd connection", func() bool { return guacd.openedCount() >= 2 })

	guacd.killAll()
	waitFor(t, 8*time.Second, "second re-attach", func() bool {
		sizes, _, _, _, _, _ := c.snapshot()
		return sizes >= 3
	})

	_, blanks, errs, _, _, _ := c.snapshot()
	if c.closed.Load() {
		t.Fatal("browser websocket must never close")
	}
	if errs != 0 {
		t.Fatalf("no error instruction may leak to a re-attach client; errs = %d", errs)
	}
	if blanks < 2 {
		t.Fatalf("expected a blank per re-attach, got %d", blanks)
	}

	_ = c.ws.Close()
	waitFor(t, 5*time.Second, "registry drained", func() bool { return gw.registry.Size() == 0 })
}

func TestFailFastWithoutReattach(t *testing.T) {
	guacd := startFakeGuacd(t)
	bridge := newFakeBridge(59006)
	gw, httpBase := startGateway(t, testConfig(guacd.addr()), bridge)

	c := dialClient(t, wsURL(httpBase, "/internal/ns/ff-vmi"))
	waitFor(t, 5*time.Second, "attached", func() bool { return gw.registry.Size() == 1 && guacd.openedCount() == 1 })

	// Coordinator-style clients must see session death (fail-fast).
	guacd.killAll()
	waitFor(t, 5*time.Second, "fail-fast close", func() bool { return c.closed.Load() })
}

// guacd announces death with an error but leaves sockets open; the gateway must
// close every member with a forwarded error and drop the session so a retry
// gets a fresh connection. Port of guacd-death.test.js.
func TestGuacdErrorDropsMembersAndSession(t *testing.T) {
	guacd := startFakeGuacd(t)
	bridge := newFakeBridge(59003)
	gw, httpBase := startGateway(t, testConfig(guacd.addr()), bridge)
	base := "/internal/ns/death-vmi"

	primary := dialClient(t, wsURL(httpBase, base))
	waitFor(t, 5*time.Second, "session open", func() bool { return gw.registry.Size() == 1 })
	joiner := dialClient(t, wsURL(httpBase, base))
	waitFor(t, 5*time.Second, "join at guacd", func() bool { return guacd.openedCount() == 2 })

	// The VM's video mode switch kills the VNC leg: guacd errors, sockets stay.
	guacd.killAll()

	waitFor(t, 5*time.Second, "members closed", func() bool {
		return primary.closed.Load() && joiner.closed.Load()
	})
	if _, _, pe, _, _, _ := primary.snapshot(); pe == 0 {
		t.Fatal("primary should receive the error instruction")
	}
	if _, _, je, _, _, _ := joiner.snapshot(); je == 0 {
		t.Fatal("joiner should receive the error instruction")
	}
	waitFor(t, 5*time.Second, "session dropped", func() bool { return gw.registry.Size() == 0 })

	// A retry must get a FRESH connection (select vnc, not a stale join).
	retry := dialClient(t, wsURL(httpBase, base))
	waitFor(t, 5*time.Second, "fresh session after retry", func() bool { return gw.registry.Size() == 1 })
	waitFor(t, 5*time.Second, "retry handshake at guacd", func() bool {
		_, id, _, _, ok := guacd.snap(2)
		return ok && id != ""
	})
	_, _, joined2, _, _ := guacd.snap(2)
	if joined2 {
		t.Fatal("retry must be a new connection, not a join")
	}
	if retry.closed.Load() {
		t.Fatal("retry connection must stay open")
	}
}
