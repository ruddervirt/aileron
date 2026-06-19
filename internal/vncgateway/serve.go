package vncgateway

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ruddervirt/aileron/internal/guac"
)

const (
	nopInstruction = "3.nop;"
	reattachDelay  = 500 * time.Millisecond
	probeTimeout   = 5 * time.Second

	// Guacamole opcodes the relay and handshake inspect/emit.
	opError      = "error"
	opDisconnect = "disconnect"
	opSize       = "size"
)

// clientConn wraps the browser/coordinator websocket. Writes are serialized
// (the hold loop, the relay's guacd→client pump, and blanking can each write,
// though never concurrently); gorilla allows only one writer at a time.
type clientConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

func (c *clientConn) writeText(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

// readLoop is the single reader for the client socket's whole lifetime. It
// forwards TEXT frames (guacamole input) to frames and signals client
// disconnect by closing closed. done lets it exit promptly when serve returns
// while it is parked on a send.
func (c *clientConn) readLoop(frames chan<- []byte, closed chan struct{}, done <-chan struct{}) {
	defer close(closed)
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue // the Guacamole protocol rides on TEXT frames
		}
		select {
		case frames <- data:
		case <-done:
			return
		}
	}
}

// lastSize tracks the most recent guacd display size seen during a session,
// used to blank the canvas before a re-attach. Written only by the guacd→client
// pump and read by serve after the relay returns (both pump goroutines have
// finished by then), so no lock is needed.
type lastSize struct{ w, h int }

// serve drives one client connection for its whole lifetime: hold until the
// console exists, attach, relay, and — with reattach — transparently re-attach
// to a fresh session whenever the guacd connection dies. Go analog of the
// former server.js attachVnc.
func (g *Gateway) serve(ws *websocket.Conn, namespace, vmi string, reattach bool) {
	cc := &clientConn{ws: ws}
	defer func() { _ = ws.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	closed := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	frames := make(chan []byte, 16)
	go cc.readLoop(frames, closed, done)
	go func() {
		<-closed
		cancel() // unblock Acquire/handshake if the client leaves
	}()

	key := namespace + "/" + vmi
	var blankW, blankH int

	for {
		if !g.waitForConsole(cc, namespace, vmi, closed, frames) {
			return // client left or hold timed out (519 already sent)
		}

		desc, err := g.registry.Acquire(ctx, namespace, vmi)
		if err != nil {
			g.log.Warn("acquire failed", "vm", key, "error", err)
			if reattach && !isClosed(closed) {
				if !sleepOrClosed(reattachDelay, closed) {
					return
				}
				continue
			}
			g.sendGuacError(cc, "Console unavailable", "519")
			return
		}

		gd, err := g.dialGuacd(desc)
		if err != nil {
			g.registry.AttachFailed(key, desc.primary, err)
			g.log.Warn("guacd handshake failed", "vm", key, "error", err)
			if reattach && !isClosed(closed) {
				if !sleepOrClosed(reattachDelay, closed) {
					return
				}
				continue
			}
			g.sendGuacError(cc, "Console unavailable", "519")
			return
		}

		if desc.primary {
			g.registry.PrimaryOpened(key, gd.connID)
		} else {
			g.registry.JoinOpened(key)
		}

		// Forward a `ready` so the client tunnel (guacamole-common-js / the
		// coordinator client) sees the connection established. guacd's own
		// `ready` was consumed during the handshake; the args/connect handshake
		// itself is guacd-side only and must not reach the client.
		_ = cc.writeText(guac.Encode("ready", gd.connID))

		ls := &lastSize{}
		guacdErr := g.relay(cc, gd, frames, closed, reattach, ls)
		g.registry.Closed(key, guacdErr)
		if ls.w > 0 && ls.h > 0 {
			blankW, blankH = ls.w, ls.h
		}

		if !reattach || isClosed(closed) {
			return // coordinator fail-fast, or the client disconnected
		}
		// Blank the canvas: the next session only paints non-black regions, so
		// leftovers from the dead session would otherwise persist.
		if blankW > 0 && blankH > 0 {
			g.blankDisplay(cc, blankW, blankH)
		}
		g.log.Info("session ended; re-attaching viewer", "vm", key)
		if !sleepOrClosed(reattachDelay, closed) {
			return
		}
	}
}

// waitForConsole holds the connection until the VM's console exists, sending
// nop keepalives (which also reset guacamole-common-js's receive timeout) and
// probing the bridge every probe interval. Returns false if the client left or
// the hold timed out (in which case a clean 519 is sent). It drains client
// frames so the single reader never parks while holding.
//
// INVARIANT: probing kicks any active VNC session (KubeVirt allows one
// connection per VMI), so probe only while registry.Has is false.
func (g *Gateway) waitForConsole(cc *clientConn, namespace, vmi string, closed <-chan struct{}, frames <-chan []byte) bool {
	if g.registry.Has(namespace, vmi) {
		return true
	}
	if err := cc.writeText([]byte(nopInstruction)); err != nil {
		return false
	}

	deadline := time.Now().Add(g.cfg.ConsoleWait)
	nextProbe := time.Now()
	for {
		if g.registry.Has(namespace, vmi) {
			return true
		}
		if !time.Now().Before(nextProbe) {
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			err := g.bridge.Probe(ctx, namespace, vmi)
			cancel()
			if err == nil {
				return !isClosed(closed)
			}
			if time.Now().After(deadline) {
				g.sendGuacError(cc, "console did not appear", "519")
				return false
			}
			nextProbe = time.Now().Add(g.cfg.ProbeInterval)
			if err := cc.writeText([]byte(nopInstruction)); err != nil {
				return false
			}
		}

		select {
		case <-closed:
			return false
		case <-frames:
			// discard client input while holding; does not advance the probe
			// timer, so a chatty client can't tighten the probe cadence.
		case <-time.After(time.Until(nextProbe)):
		}
	}
}

// relay bridges the client websocket and the guacd connection until either side
// ends. It returns true if guacd announced a fatal error/disconnect (so the
// registry drops the whole session). In reattach mode the terminal instruction
// is swallowed (the serve loop re-attaches); in fail-fast mode it is forwarded.
func (g *Gateway) relay(cc *clientConn, gd *guacdConn, frames <-chan []byte, closed <-chan struct{}, reattach bool, ls *lastSize) bool {
	errCh := make(chan bool, 1)
	go g.pumpGuacdToClient(cc, gd, reattach, ls, errCh)

	for {
		select {
		case sawErr := <-errCh:
			return sawErr
		case <-closed:
			gd.close()
			return <-errCh
		case f := <-frames:
			if err := gd.send(f); err != nil {
				gd.close()
				return <-errCh
			}
		}
	}
}

// pumpGuacdToClient reads the guacd display stream and forwards it to the
// client, inspecting opcodes for control decisions. It signals completion on
// errCh (true if a guacd error/disconnect was seen).
func (g *Gateway) pumpGuacdToClient(cc *clientConn, gd *guacdConn, reattach bool, ls *lastSize, errCh chan<- bool) {
	sawErr := false
	defer func() {
		gd.close()
		errCh <- sawErr
	}()

	for {
		ins, raw, err := gd.readInstruction()
		if err != nil {
			return // guacd EOF/socket error
		}
		switch ins.Opcode {
		case opError, opDisconnect:
			// guacd announces connection death but often leaves sockets open;
			// treat it as fatal. Forward it only in fail-fast mode — in reattach
			// mode it would tear down guacamole-common-js and defeat the
			// transparent re-attach.
			sawErr = true
			if !reattach {
				_ = cc.writeText(raw)
			}
			return
		case opSize:
			// size,<layer>,<W>,<H>: track layer-0 geometry for blanking.
			if len(ins.Args) >= 3 && ins.Args[0] == "0" {
				if w, err := strconv.Atoi(ins.Args[1]); err == nil {
					if h, err := strconv.Atoi(ins.Args[2]); err == nil && w > 0 && h > 0 {
						ls.w, ls.h = w, h
					}
				}
			}
			if err := cc.writeText(raw); err != nil {
				return
			}
		default:
			if err := cc.writeText(raw); err != nil {
				return
			}
		}
	}
}

// sendGuacError forwards a Guacamole error instruction so guacamole-common-js
// surfaces a proper tunnel error (5.error,<message>,<code>;).
func (g *Gateway) sendGuacError(cc *clientConn, message, code string) {
	_ = cc.writeText(guac.Encode(opError, message, code))
}

// blankDisplay paints layer 0 opaque black (composite mode 14 = SRC_OVER) so
// content from a dead session doesn't bleed into the next one. Byte-identical
// to the former server.js blankDisplay.
func (g *Gateway) blankDisplay(cc *clientConn, width, height int) {
	_ = cc.writeText(guac.Encode("rect", "0", "0", "0", strconv.Itoa(width), strconv.Itoa(height)))
	_ = cc.writeText(guac.Encode("cfill", "14", "0", "0", "0", "0", "255"))
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// sleepOrClosed waits for d, returning false early if closed fires.
func sleepOrClosed(d time.Duration, closed <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-closed:
		return false
	}
}
