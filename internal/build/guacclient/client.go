package guacclient

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// opSync is the Guacamole ping instruction; clients must echo its timestamp
// or guacd drops them.
const opSync = "sync"

// Client is a Guacamole protocol client connected through the vncgateway.
// It sends input instructions and answers server sync pings; all display
// output (img/blob/...) is discarded.
type Client struct {
	ws *websocket.Conn

	writeMu sync.Mutex

	readyOnce sync.Once
	ready     chan struct{}
	done      chan struct{}

	errMu   sync.Mutex
	readErr error
}

// Dial connects to a vncgateway WebSocket endpoint and waits until the first
// display instruction (size/img/...) arrives. guacd sends ready/sync before
// its upstream VNC connection is actually attached, so only display output
// proves the VMI console is live — the old raw-RFB handshake gave the same
// guarantee. A WebSocket that closes earlier is a retryable failure (e.g.
// the VMI is not up yet).
func Dial(ctx context.Context, wsURL string) (*Client, error) {
	dialer := websocket.Dialer{
		Subprotocols:     []string{"guacamole"},
		HandshakeTimeout: 5 * time.Second,
	}
	ws, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", wsURL, err)
	}

	c := &Client{
		ws:    ws,
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
	go c.readLoop()

	select {
	case <-c.ready:
		return c, nil
	case <-c.done:
		_ = ws.Close()
		return nil, fmt.Errorf("connection closed before ready: %w", c.err())
	case <-ctx.Done():
		_ = ws.Close()
		return nil, ctx.Err()
	}
}

// SendKey sends one key press or release with an X11 keysym.
func (c *Client) SendKey(keysym uint32, down bool) error {
	pressed := "0"
	if down {
		pressed = "1"
	}
	return c.write(Encode("key", strconv.FormatUint(uint64(keysym), 10), pressed))
}

// Close tears down the connection.
func (c *Client) Close() error {
	return c.ws.Close()
}

func (c *Client) err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.readErr
}

func (c *Client) setErr(err error) {
	c.errMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.errMu.Unlock()
}

func (c *Client) write(data []byte) error {
	select {
	case <-c.done:
		return fmt.Errorf("guac connection closed: %w", c.err())
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) readLoop() {
	// Close the socket too: the gateway tears the session down on close, and
	// leaving it open after a fatal error (e.g. a guacd error instruction)
	// would hold a zombie session that blocks other viewers.
	defer func() {
		_ = c.ws.Close()
		close(c.done)
	}()
	var dec Decoder
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.setErr(fmt.Errorf("websocket read: %w", err))
			c.logEnd("read", err)
			return
		}
		dec.Feed(data)
		for {
			ins, derr := dec.Next()
			if derr != nil {
				c.setErr(derr)
				c.logEnd("decode", derr)
				return
			}
			if ins == nil {
				break
			}
			if err := c.handle(ins); err != nil {
				c.setErr(err)
				c.logEnd("instruction "+ins.Opcode, err)
				return
			}
		}
	}
}

// logEnd reports why the read loop died. Deaths before readiness are the
// normal Dial-polling rhythm while the VMI console isn't up yet — keep those
// quiet; deaths of an established session are worth a warning.
func (c *Client) logEnd(cause string, err error) {
	select {
	case <-c.ready:
		slog.Warn("guac session ended", "cause", cause, "error", err)
	default:
		slog.Debug("guac connection attempt ended before console ready", "cause", cause, "error", err)
	}
}

func (c *Client) handle(ins *Instruction) error {
	switch ins.Opcode {
	case opSync:
		// guacd drops clients that never acknowledge sync; echo the timestamp.
		if len(ins.Args) > 0 {
			return c.write(Encode(opSync, ins.Args[0]))
		}
	case "ready", "nop", "args", "":
		// Control traffic emitted before the VNC leg is attached — not proof
		// of a live console. "" is the connection-ID instruction (the
		// Guacamole protocol uses an empty opcode for it), which
		// guacamole-lite forwards immediately after the guacd handshake.
	case "error":
		// guacd announces a dying connection (e.g. upstream VNC lost) with an
		// error instruction before closing. Fail fast so the caller's
		// reconnect path kicks in instead of typing keys into a dead session.
		return fmt.Errorf("server error instruction: %v", ins.Args)
	case "disconnect":
		return fmt.Errorf("server sent disconnect")
	default:
		// Display output (size/img/blob/copy/cursor/name/...): the VNC
		// connection to the VMI is live.
		c.readyOnce.Do(func() { close(c.ready) })
	}
	return nil
}
