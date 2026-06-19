package vncgateway

import (
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ruddervirt/aileron/internal/guac"
)

// guacdConn is a single TCP connection to guacd that has completed the
// Guacamole handshake. It replaces what guacamole-lite's GuacdClient did
// internally. One decoder is shared by the handshake and the relay so any
// display bytes guacd sends immediately after `ready` are not lost.
type guacdConn struct {
	conn   net.Conn
	dec    guac.Decoder
	rbuf   []byte
	connID string // the "$..." id guacd assigns; needed for joins
}

// Handshake instruction defaults. These describe the gateway (acting as the
// guacd client) to guacd; guacd's own VNC-leg encode is driven by the connect
// parameters (encodings, quality-level, ...). The image mimetypes must include
// what guacd may emit and the browser can decode — png+jpeg covers guacd 1.5.5
// with quality-level/force-lossless. (Validate against real guacd 1.5.5 with
// the smoke test; this is the one spot the fake guacd can't catch — see
// docs/vncgateway.md risk notes.)
var (
	handshakeSize    = []string{"1024", "768", "96"}
	handshakeAudio   = []string{}
	handshakeVideo   = []string{}
	handshakeImage   = []string{"image/png", "image/jpeg"}
	guacdDialTimeout = 5 * time.Second
)

// dialGuacd opens a TCP connection to guacd and performs the handshake for the
// given attach: a CONNECT (`select vnc`) for the primary with the bridge tunnel
// port, or a JOIN (`select $connID`) for a later viewer. On success the
// returned conn is positioned at the start of the display stream.
func (g *Gateway) dialGuacd(desc acquireResult) (*guacdConn, error) {
	conn, err := net.DialTimeout("tcp", g.cfg.GuacdAddr, guacdDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial guacd %s: %w", g.cfg.GuacdAddr, err)
	}
	gd := &guacdConn{conn: conn, rbuf: make([]byte, 32*1024)}

	selectArg := "vnc"
	var settings map[string]string
	if desc.primary {
		settings = maps.Clone(g.cfg.ConnectSettings)
		settings["hostname"] = "127.0.0.1"
		settings["port"] = strconv.Itoa(desc.port)
	} else {
		selectArg = desc.connID
		settings = g.cfg.JoinSettings
	}

	if err := gd.handshake(selectArg, settings); err != nil {
		_ = gd.conn.Close()
		return nil, err
	}
	return gd, nil
}

// handshake performs the guacd-side Guacamole handshake on an open connection:
// select -> args, then size/audio/video/image, then connect with values aligned
// to the advertised args, then ready (capturing the connection id). Split out
// from dialGuacd so the smoke test can drive it against a real guacd.
func (gd *guacdConn) handshake(selectArg string, settings map[string]string) error {
	if err := gd.send(guac.Encode("select", selectArg)); err != nil {
		return fmt.Errorf("guacd select: %w", err)
	}

	// guacd replies with `args`: arg[0] is the protocol version, the rest are
	// the connection parameter names, in the order guacd wants their values.
	argsIns, err := gd.readUntil("args")
	if err != nil {
		return err
	}

	for _, ins := range [][]byte{
		guac.Encode(opSize, handshakeSize...),
		guac.Encode("audio", handshakeAudio...),
		guac.Encode("video", handshakeVideo...),
		guac.Encode("image", handshakeImage...),
	} {
		if err := gd.send(ins); err != nil {
			return fmt.Errorf("guacd handshake: %w", err)
		}
	}

	// connect carries one value per advertised arg, in order. guacd 1.1.0+
	// advertises its protocol version as the first arg (VERSION_x_y_z); the
	// client echoes it back at the same position to confirm it speaks that
	// version, then maps the remaining names to settings (unknown -> empty).
	advertised := argsIns.Args
	vals := make([]string, len(advertised))
	for i, name := range advertised {
		if i == 0 && strings.HasPrefix(name, "VERSION_") {
			vals[i] = name
			continue
		}
		vals[i] = settings[name]
	}
	if err := gd.send(guac.Encode("connect", vals...)); err != nil {
		return fmt.Errorf("guacd connect: %w", err)
	}

	readyIns, err := gd.readUntil("ready")
	if err != nil {
		return err
	}
	if len(readyIns.Args) > 0 {
		gd.connID = readyIns.Args[0]
	}
	return nil
}

func (gd *guacdConn) send(b []byte) error {
	_, err := gd.conn.Write(b)
	return err
}

// readUntil reads instructions until one with the given opcode arrives,
// skipping unrelated control traffic (nop, ...). A guacd error/disconnect
// during the handshake is returned as an error.
func (gd *guacdConn) readUntil(opcode string) (*guac.Instruction, error) {
	for {
		ins, _, err := gd.readInstruction()
		if err != nil {
			return nil, err
		}
		switch ins.Opcode {
		case opcode:
			return ins, nil
		case opError, opDisconnect:
			return nil, fmt.Errorf("guacd %s during handshake: %v", ins.Opcode, ins.Args)
		default:
			// e.g. nop — keep reading.
		}
	}
}

// readInstruction returns the next decoded instruction and its raw wire bytes,
// reading from the socket as needed. The same decoder is used by the relay, so
// bytes buffered past one instruction survive into the next read. The raw slice
// is valid only until the next readInstruction call (it aliases the decoder
// buffer); the relay forwards it before reading again.
func (gd *guacdConn) readInstruction() (*guac.Instruction, []byte, error) {
	for {
		ins, raw, err := gd.dec.NextRaw()
		if err != nil {
			return nil, nil, err
		}
		if ins != nil {
			return ins, raw, nil
		}
		n, err := gd.conn.Read(gd.rbuf)
		if err != nil {
			return nil, nil, err
		}
		gd.dec.Feed(gd.rbuf[:n])
	}
}

func (gd *guacdConn) close() { _ = gd.conn.Close() }
