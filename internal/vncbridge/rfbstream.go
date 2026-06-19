package vncbridge

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
)

// rfbStreamTracker passively observes both directions of a tunneled RFB
// connection and reports framebuffer geometry changes (DesktopSize /
// ExtendedDesktopSize pseudo-rects). KubeVirt allows exactly ONE VNC
// connection per VMI, so the size cannot be watched out-of-band — it must be
// read from the single stream that already flows through the bridge.
//
// Safety property: the tracker is a best-effort TEE. Bytes are handed to it
// through an async buffer; if parsing desyncs (unknown encoding/message) or
// the parser cannot keep up, detection is disabled for the connection and the
// pipe itself is never disturbed.
//
// Parsing only needs to know each message's LENGTH. The encodings guacd
// negotiates are constrained via the gateway's GUAC_ENCODINGS setting (zrle
// copyrect by default) plus the pseudo-encodings libvncclient always
// advertises — the same set the old aileron vncproxy parser handled.
// CONTRACT: if GUAC_ENCODINGS is ever widened to an encoding without a
// deterministic wire length (tight!), detection silently turns off and
// viewers stop following guest resolution changes. See docs/vncgateway.md.

// RFB encodings (subset).
const (
	encRaw                  = 0
	encCopyRect             = 1
	encZlib                 = 6
	encZRLE                 = 16
	encLastRectPseudo       = -224
	encDesktopSizePseudo    = -223
	encExtDesktopSizePseudo = -308
	encCursorPseudo         = -239
	encXCursorPseudo        = -240
	encDesktopNamePseudo    = -307
	encLedStatePseudo       = -261
	encQEMUExtKeyPseudo     = -258
	encQEMUAudioPseudo      = -259
)

type rfbStreamTracker struct {
	key      string
	onResize func(w, h uint16)

	// bytes-per-pixel shared between the directions: ServerInit sets it,
	// client SetPixelFormat updates it.
	bypp atomic.Int32

	server *asyncFeed
	client *asyncFeed

	w, h uint16 // last known geometry (server parser goroutine only)
}

func newRFBStreamTracker(key string, onResize func(w, h uint16)) *rfbStreamTracker {
	t := &rfbStreamTracker{key: key, onResize: onResize}
	t.bypp.Store(4)
	t.server = newAsyncFeed(key + "/server")
	t.client = newAsyncFeed(key + "/client")
	go t.parseServer(t.server)
	go t.parseClient(t.client)
	return t
}

// ServerWriter receives a copy of server->client bytes.
func (t *rfbStreamTracker) ServerWriter() io.Writer { return t.server }

// ClientWriter receives a copy of client->server bytes.
func (t *rfbStreamTracker) ClientWriter() io.Writer { return t.client }

func (t *rfbStreamTracker) Close() {
	t.server.close()
	t.client.close()
}

func (t *rfbStreamTracker) parseServer(feed *asyncFeed) {
	defer feed.drain()
	r := feed.reader()

	// Handshake: version(12), nsec(1)+types, security result(4),
	// ServerInit(24+name).
	var version [12]byte
	if _, err := io.ReadFull(r, version[:]); err != nil {
		return
	}
	var nsec [1]byte
	if _, err := io.ReadFull(r, nsec[:]); err != nil {
		return
	}
	if _, err := io.CopyN(io.Discard, r, int64(nsec[0])+4); err != nil {
		return
	}
	var serverInit [24]byte
	if _, err := io.ReadFull(r, serverInit[:]); err != nil {
		return
	}
	t.w = binary.BigEndian.Uint16(serverInit[0:])
	t.h = binary.BigEndian.Uint16(serverInit[2:])
	if b := int32(serverInit[4]) / 8; b > 0 {
		t.bypp.Store(b)
	}
	nameLen := binary.BigEndian.Uint32(serverInit[20:])
	if _, err := io.CopyN(io.Discard, r, int64(nameLen)); err != nil {
		return
	}

	for {
		if err := t.readServerMessage(r); err != nil {
			if err != io.EOF && err != io.ErrClosedPipe {
				slog.Warn("rfb tracker: server stream detection disabled",
					"tunnel", t.key, "error", err)
			}
			return
		}
	}
}

func (t *rfbStreamTracker) readServerMessage(r io.Reader) error {
	var msgType [1]byte
	if _, err := io.ReadFull(r, msgType[:]); err != nil {
		return err
	}
	switch msgType[0] {
	case 0: // FramebufferUpdate
		return t.readFramebufferUpdate(r)
	case 1: // SetColourMapEntries: pad(1)+first(2)+n(2), then n*6
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint16(hdr[3:])
		_, err := io.CopyN(io.Discard, r, int64(n)*6)
		return err
	case 2: // Bell
		return nil
	case 3: // ServerCutText: pad(3)+len(4)+data
		var hdr [7]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(hdr[3:])
		_, err := io.CopyN(io.Discard, r, int64(n))
		return err
	case 248: // ServerFence: pad(3)+flags(4)+len(1)+payload
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		_, err := io.CopyN(io.Discard, r, int64(hdr[7]))
		return err
	default:
		return fmt.Errorf("unsupported server message %d", msgType[0])
	}
}

func (t *rfbStreamTracker) readFramebufferUpdate(r io.Reader) error {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	nrects := binary.BigEndian.Uint16(hdr[1:])
	for range int(nrects) {
		var rect [12]byte
		if _, err := io.ReadFull(r, rect[:]); err != nil {
			return err
		}
		w := binary.BigEndian.Uint16(rect[4:])
		h := binary.BigEndian.Uint16(rect[6:])
		enc := int32(binary.BigEndian.Uint32(rect[8:]))

		if enc == encLastRectPseudo {
			return nil // terminates the update regardless of declared count
		}
		if err := t.skipEncodingData(r, enc, w, h); err != nil {
			return err
		}
		if enc == encDesktopSizePseudo || enc == encExtDesktopSizePseudo {
			if w != t.w || h != t.h {
				t.w, t.h = w, h
				if t.onResize != nil {
					t.onResize(w, h)
				}
			}
		}
	}
	return nil
}

func (t *rfbStreamTracker) skipEncodingData(r io.Reader, enc int32, w, h uint16) error {
	bypp := int64(t.bypp.Load())
	switch enc {
	case encRaw:
		_, err := io.CopyN(io.Discard, r, int64(w)*int64(h)*bypp)
		return err
	case encCopyRect:
		_, err := io.CopyN(io.Discard, r, 4)
		return err
	case encZlib, encZRLE:
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return err
		}
		_, err := io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(lenBuf[:])))
		return err
	case encDesktopSizePseudo, encQEMUExtKeyPseudo, encQEMUAudioPseudo:
		return nil // zero-payload pseudo-rects (capability acks / size in header)
	case encExtDesktopSizePseudo:
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		_, err := io.CopyN(io.Discard, r, int64(hdr[0])*16)
		return err
	case encCursorPseudo:
		if w == 0 || h == 0 {
			return nil
		}
		pixels := int64(w) * int64(h) * bypp
		bitmask := int64(((int(w) + 7) / 8) * int(h))
		_, err := io.CopyN(io.Discard, r, pixels+bitmask)
		return err
	case encXCursorPseudo:
		if w == 0 || h == 0 {
			return nil
		}
		rowBytes := int64((int(w) + 7) / 8)
		_, err := io.CopyN(io.Discard, r, 6+rowBytes*int64(h)*2)
		return err
	case encDesktopNamePseudo:
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return err
		}
		_, err := io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(lenBuf[:])))
		return err
	case encLedStatePseudo:
		_, err := io.CopyN(io.Discard, r, 1)
		return err
	default:
		return fmt.Errorf("unsupported encoding %d", enc)
	}
}

func (t *rfbStreamTracker) parseClient(feed *asyncFeed) {
	defer feed.drain()
	r := feed.reader()

	// Handshake: version(12), security choice(1), ClientInit(1).
	if _, err := io.CopyN(io.Discard, r, 14); err != nil {
		return
	}
	for {
		if err := t.readClientMessage(r); err != nil {
			if err != io.EOF && err != io.ErrClosedPipe {
				slog.Warn("rfb tracker: client stream detection disabled",
					"tunnel", t.key, "error", err)
			}
			return
		}
	}
}

func (t *rfbStreamTracker) readClientMessage(r io.Reader) error {
	var msgType [1]byte
	if _, err := io.ReadFull(r, msgType[:]); err != nil {
		return err
	}
	switch msgType[0] {
	case 0: // SetPixelFormat: pad(3)+pixfmt(16)
		var rest [19]byte
		if _, err := io.ReadFull(r, rest[:]); err != nil {
			return err
		}
		if b := int32(rest[3]) / 8; b > 0 {
			t.bypp.Store(b)
		}
		return nil
	case 2: // SetEncodings: pad(1)+n(2)+n*4
		var hdr [3]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint16(hdr[1:])
		_, err := io.CopyN(io.Discard, r, int64(n)*4)
		return err
	case 3: // FramebufferUpdateRequest
		_, err := io.CopyN(io.Discard, r, 9)
		return err
	case 4: // KeyEvent
		_, err := io.CopyN(io.Discard, r, 7)
		return err
	case 5: // PointerEvent
		_, err := io.CopyN(io.Discard, r, 5)
		return err
	case 6: // ClientCutText: pad(3)+len(4)+data
		var hdr [7]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(hdr[3:])
		_, err := io.CopyN(io.Discard, r, int64(n))
		return err
	case 255: // QEMU client messages: submessage 0 = extended key event
		var sub [1]byte
		if _, err := io.ReadFull(r, sub[:]); err != nil {
			return err
		}
		if sub[0] != 0 {
			return fmt.Errorf("unsupported QEMU client submessage %d", sub[0])
		}
		_, err := io.CopyN(io.Discard, r, 10) // down(2)+keysym(4)+keycode(4)
		return err
	default:
		return fmt.Errorf("unsupported client message %d", msgType[0])
	}
}

// asyncFeed decouples the tunnel pipe from the parser: Write never blocks on
// parsing. If the parser falls behind (buffer full) or has stopped, bytes are
// dropped and detection is disabled — the pipe itself is unaffected.
type asyncFeed struct {
	ch     chan []byte
	dead   atomic.Bool
	label  string
	warned atomic.Bool
}

func newAsyncFeed(label string) *asyncFeed {
	return &asyncFeed{ch: make(chan []byte, 256), label: label}
}

func (f *asyncFeed) Write(p []byte) (int, error) {
	if f.dead.Load() {
		return len(p), nil
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case f.ch <- cp:
	default:
		f.dead.Store(true)
		if f.warned.CompareAndSwap(false, true) {
			slog.Warn("rfb tracker: parser fell behind, detection disabled", "feed", f.label)
		}
	}
	return len(p), nil
}

// close signals EOF to the parser via a nil sentinel. The channel itself is
// never closed, so Write can never panic on a racing send.
func (f *asyncFeed) close() {
	f.dead.Store(true)
	select {
	case f.ch <- nil:
	default:
	}
}

// reader exposes the fed bytes as an io.Reader for the parser goroutine.
func (f *asyncFeed) reader() io.Reader {
	return &feedReader{f: f}
}

// drain marks the feed dead so writers stop queueing, and empties anything
// already buffered.
func (f *asyncFeed) drain() {
	f.dead.Store(true)
	for {
		select {
		case <-f.ch:
		default:
			return
		}
	}
}

type feedReader struct {
	f   *asyncFeed
	cur []byte
}

func (r *feedReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		b := <-r.f.ch
		if b == nil {
			return 0, io.EOF
		}
		r.cur = b
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}
