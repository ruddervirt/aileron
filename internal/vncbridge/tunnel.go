package vncbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// tunnel owns one localhost TCP listener for one VMI. Each accepted TCP
// connection is piped to a KubeVirt VNC WebSocket.
//
// IMPORTANT: KubeVirt allows exactly ONE VNC connection per VMI — a new
// connection silently kicks the existing one mid-stream. In normal operation
// the gateway therefore funnels ALL viewers through a single guacd
// connection (Guacamole joins), so a tunnel has one active conn at a time.
// The listener still accepts whatever arrives (a second accept during a
// recycle race is normal); it must simply never be the layer that ASSUMES
// concurrency is safe. See vncgateway/vncgateway.md.
type tunnel struct {
	key       string
	namespace string
	vmi       string
	ln        net.Listener
	bridge    *Bridge

	mu        sync.Mutex
	active    int
	closed    bool
	idleTimer *time.Timer
	conns     map[net.Conn]struct{}
}

func (t *tunnel) port() int {
	return t.ln.Addr().(*net.TCPAddr).Port
}

func (t *tunnel) acceptLoop() {
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			return // listener closed
		}

		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			_ = conn.Close()
			continue
		}
		t.active++
		t.idleTimer.Stop()
		if t.conns == nil {
			t.conns = make(map[net.Conn]struct{})
		}
		t.conns[conn] = struct{}{}
		t.mu.Unlock()

		go t.serve(conn)
	}
}

func (t *tunnel) serve(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		t.mu.Lock()
		t.active--
		delete(t.conns, conn)
		if t.active == 0 && !t.closed {
			t.idleTimer.Reset(t.bridge.idleTimeout)
		}
		t.mu.Unlock()
	}()

	ws, err := t.bridge.dial(context.Background(), t.namespace, t.vmi)
	if err != nil {
		slog.Error("KubeVirt VNC dial failed", "tunnel", t.key, "error", err)
		return
	}
	defer func() { _ = ws.Close() }()

	slog.Info("tunnel connection established", "tunnel", t.key, "remote", conn.RemoteAddr())

	// Passive geometry detection: guacd does not propagate framebuffer
	// SHRINKS to viewers, so when the guest changes resolution the bridge
	// recycles the connection — every viewer re-attaches (blank + fresh
	// session) at the exact new size.
	tracker := newRFBStreamTracker(t.key, func(w, h uint16) {
		slog.Info("framebuffer size changed; recycling tunnel connections",
			"tunnel", t.key, "to", fmt.Sprintf("%dx%d", w, h))
		t.killConns()
	})
	defer tracker.Close()

	done := make(chan struct{}, 2)

	// TCP -> WS
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				_, _ = tracker.ClientWriter().Write(buf[:n])
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// WS -> TCP
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, r, err := ws.NextReader()
			if err != nil {
				return
			}
			if msgType != websocket.BinaryMessage {
				_, _ = io.Copy(io.Discard, r)
				continue
			}
			if _, err := io.Copy(conn, io.TeeReader(r, tracker.ServerWriter())); err != nil {
				return
			}
		}
	}()

	<-done
	// Closing both ends unblocks the other goroutine.
	_ = conn.Close()
	_ = ws.Close()
	<-done

	slog.Info("tunnel connection closed", "tunnel", t.key)
}

// reap closes the tunnel if it has been idle for the full timeout. A
// connection accepted between the timer firing and the lock being taken
// cancels the reap.
func (t *tunnel) reap() {
	t.mu.Lock()
	if t.closed || t.active > 0 {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()

	t.bridge.remove(t)
	_ = t.ln.Close()
	slog.Info("reaped idle tunnel", "tunnel", t.key)
}

// killConns closes every active connection (their serve goroutines clean
// up). Used when the guest framebuffer geometry changes: viewers re-attach
// and get fresh sessions at the new size.
func (t *tunnel) killConns() {
	t.mu.Lock()
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// close tears the tunnel down regardless of activity (explicit DELETE).
func (t *tunnel) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.idleTimer.Stop()
	t.mu.Unlock()

	_ = t.ln.Close()
}

var errBridgeClosed = errors.New("bridge closed")
