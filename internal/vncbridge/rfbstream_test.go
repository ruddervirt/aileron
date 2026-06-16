package vncbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeVNC is a minimal RFB 3.8 server over websocket (standing in for the
// KubeVirt VNC subresource) whose framebuffer size can be changed at runtime.
type fakeVNC struct {
	srv *httptest.Server

	mu sync.Mutex
	w  uint16
	h  uint16
}

func (f *fakeVNC) size() (uint16, uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, f.h
}

func (f *fakeVNC) setSize(w, h uint16) {
	f.mu.Lock()
	f.w, f.h = w, h
	f.mu.Unlock()
}

func newFakeVNC(t *testing.T, w, h uint16) *fakeVNC {
	t.Helper()
	f := &fakeVNC{w: w, h: h}
	upgrader := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		ws, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		f.serveRFB(ws)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVNC) serveRFB(ws *websocket.Conn) {
	r := &wsByteReader{ws: ws}
	write := func(b []byte) error { return ws.WriteMessage(websocket.BinaryMessage, b) }

	if write([]byte("RFB 003.008\n")) != nil {
		return
	}
	clientVersion := make([]byte, 12)
	if _, err := io.ReadFull(r, clientVersion); err != nil {
		return
	}
	if write([]byte{1, 1}) != nil { // one security type: None
		return
	}
	choice := make([]byte, 1)
	if _, err := io.ReadFull(r, choice); err != nil {
		return
	}
	if write([]byte{0, 0, 0, 0}) != nil { // security OK
		return
	}
	if _, err := io.ReadFull(r, choice); err != nil { // ClientInit
		return
	}
	announcedW, announcedH := f.size()
	serverInit := make([]byte, 24)
	binary.BigEndian.PutUint16(serverInit[0:], announcedW)
	binary.BigEndian.PutUint16(serverInit[2:], announcedH)
	serverInit[4] = 32 // bpp
	if write(serverInit) != nil {
		return
	}

	sendDesktopSize := func(w, h uint16) error {
		msg := make([]byte, 16)
		// FramebufferUpdate, 1 rect: x=0,y=0,w,h,encoding=-223
		binary.BigEndian.PutUint16(msg[2:], 1)
		binary.BigEndian.PutUint16(msg[8:], w)
		binary.BigEndian.PutUint16(msg[10:], h)
		enc := int32(encDesktopSizePseudo)
		binary.BigEndian.PutUint32(msg[12:], uint32(enc))
		return write(msg)
	}
	sendRawPixel := func() error {
		msg := make([]byte, 16+4)
		binary.BigEndian.PutUint16(msg[2:], 1)
		binary.BigEndian.PutUint16(msg[8:], 1)  // w
		binary.BigEndian.PutUint16(msg[10:], 1) // h
		// encoding 0 = Raw, 4 bytes of pixel
		return write(msg)
	}

	for {
		msgType := make([]byte, 1)
		if _, err := io.ReadFull(r, msgType); err != nil {
			return
		}
		switch msgType[0] {
		case 2: // SetEncodings
			hdr := make([]byte, 3)
			if _, err := io.ReadFull(r, hdr); err != nil {
				return
			}
			n := binary.BigEndian.Uint16(hdr[1:])
			if _, err := io.CopyN(io.Discard, r, int64(n)*4); err != nil {
				return
			}
		case 3: // FramebufferUpdateRequest
			req := make([]byte, 9)
			if _, err := io.ReadFull(r, req); err != nil {
				return
			}
			incremental := req[0] == 1
			if !incremental {
				if sendRawPixel() != nil {
					return
				}
				continue
			}
			// Incremental: block until the size changes (poll), like a real
			// server holding the response until there is something to send.
			for {
				w, h := f.size()
				if w != announcedW || h != announcedH {
					announcedW, announcedH = w, h
					if sendDesktopSize(w, h) != nil {
						return
					}
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		default:
			return
		}
	}
}

func fakeVNCDialer(f *fakeVNC) dialFunc {
	wsURL := "ws" + strings.TrimPrefix(f.srv.URL, "http")
	return func(ctx context.Context, namespace, vmi string) (*websocket.Conn, error) {
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		return ws, err
	}
}

// rfbClientHandshake performs the client side of the handshake through a
// tunnel pipe, returning the ServerInit width/height.
func rfbClientHandshake(t *testing.T, conn net.Conn) (uint16, uint16) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	version := make([]byte, 12)
	if err := readFull(conn, version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatalf("write version: %v", err)
	}
	sec := make([]byte, 2)
	if err := readFull(conn, sec); err != nil {
		t.Fatalf("read security: %v", err)
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatalf("choose security: %v", err)
	}
	result := make([]byte, 4)
	if err := readFull(conn, result); err != nil {
		t.Fatalf("read security result: %v", err)
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatalf("client init: %v", err)
	}
	serverInit := make([]byte, 24)
	if err := readFull(conn, serverInit); err != nil {
		t.Fatalf("read server init: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return binary.BigEndian.Uint16(serverInit[0:]), binary.BigEndian.Uint16(serverInit[2:])
}

// wsByteReader adapts the websocket binary-message stream to io.Reader
// (test helper for the fake VNC server).
type wsByteReader struct {
	ws  *websocket.Conn
	cur io.Reader
}

func (r *wsByteReader) Read(p []byte) (int, error) {
	for {
		if r.cur != nil {
			n, err := r.cur.Read(p)
			if err == io.EOF {
				r.cur = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}
		msgType, rd, err := r.ws.NextReader()
		if err != nil {
			return 0, err
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		r.cur = rd
	}
}

// TestTrackerRecyclesOnResize drives a real RFB exchange through the tunnel
// pipe: the passive stream tracker must detect the DesktopSize pseudo-rect
// and recycle the connection.
func TestTrackerRecyclesOnResize(t *testing.T) {
	fake := newFakeVNC(t, 1024, 768)
	b := newWithDialer(fakeVNCDialer(fake), time.Minute)
	defer b.Close()

	port, err := b.EnsureTunnel(context.Background(), "ns", "vmi-t")
	if err != nil {
		t.Fatalf("EnsureTunnel: %v", err)
	}
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if w, h := rfbClientHandshake(t, conn); w != 1024 || h != 768 {
		t.Fatalf("unexpected geometry %dx%d", w, h)
	}

	fbur := func(incremental byte) {
		req := make([]byte, 10)
		req[0] = 3
		req[1] = incremental
		binary.BigEndian.PutUint16(req[6:], 1)
		binary.BigEndian.PutUint16(req[8:], 1)
		if _, err := conn.Write(req); err != nil {
			t.Fatalf("write FBUR: %v", err)
		}
	}

	// Non-incremental request: fake replies with a 1x1 Raw update (16+4
	// bytes) that the tracker must parse through cleanly.
	fbur(0)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	update := make([]byte, 20)
	if err := readFull(conn, update); err != nil {
		t.Fatalf("read raw update: %v", err)
	}

	// Incremental request blocks server-side until the size changes.
	fbur(1)
	time.Sleep(200 * time.Millisecond)
	fake.setSize(640, 480)

	// The fake sends a DesktopSize rect; the tracker must kill the conn.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := conn.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("connection still alive after resize")
			}
			return // recycled — success
		}
	}
}
