package build

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/rest"
)

// VNCClient is a minimal RFB 3.8 client that can send key events over a
// WebSocket connection to a KubeVirt VNC subresource. It does NOT render
// pixels — it only sends keyboard input.
type VNCClient struct {
	ws *websocket.Conn
}

// DialVNC opens a VNC WebSocket to the given VMI's VNC subresource and
// performs the RFB 3.8 handshake.
func DialVNC(ctx context.Context, restConfig *rest.Config, namespace, vmiName string) (*VNCClient, error) {
	wsURL, err := vncWebSocketURL(restConfig, namespace, vmiName)
	if err != nil {
		return nil, fmt.Errorf("building VNC URL: %w", err)
	}

	dialer := websocket.Dialer{
		Subprotocols: []string{"binary"},
	}

	// TLS configuration from rest.Config.
	tlsConfig := &tls.Config{}
	if restConfig.Insecure {
		tlsConfig.InsecureSkipVerify = true
	}
	if restConfig.CAFile != "" || len(restConfig.CAData) > 0 {
		pool, err := rest.TLSConfigFor(restConfig)
		if err == nil && pool != nil {
			tlsConfig = pool
		}
	}
	dialer.TLSClientConfig = tlsConfig

	// Auth header.
	header := http.Header{}
	token := restConfig.BearerToken
	if token == "" && restConfig.BearerTokenFile != "" {
		data, err := os.ReadFile(restConfig.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading bearer token file: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}

	ws, _, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return nil, fmt.Errorf("WebSocket dial to VNC: %w", err)
	}

	c := &VNCClient{ws: ws}
	if err := c.handshake(); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("RFB handshake: %w", err)
	}
	return c, nil
}

// Close closes the underlying WebSocket connection.
func (c *VNCClient) Close() error {
	return c.ws.Close()
}

// SendKeyEvent sends an RFB KeyEvent message (message type 4).
// down=true means key pressed, down=false means key released.
func (c *VNCClient) SendKeyEvent(keysym uint32, down bool) error {
	var msg [8]byte
	msg[0] = 4 // KeyEvent message type
	if down {
		msg[1] = 1
	}
	// bytes 2-3 are padding
	binary.BigEndian.PutUint32(msg[4:], keysym)
	return c.ws.WriteMessage(websocket.BinaryMessage, msg[:])
}

// handshake performs a minimal RFB 3.8 handshake: version exchange,
// security type 1 (None), ClientInit, and ServerInit.
func (c *VNCClient) handshake() error {
	r := &wsReader{ws: c.ws}

	// --- ProtocolVersion ---
	var serverVersion [12]byte
	if _, err := io.ReadFull(r, serverVersion[:]); err != nil {
		return fmt.Errorf("reading server version: %w", err)
	}
	// Reply with RFB 003.008.
	if err := c.ws.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n")); err != nil {
		return fmt.Errorf("sending client version: %w", err)
	}

	// --- Security ---
	// Server sends: number-of-security-types (1 byte) then the types.
	var numSecTypes [1]byte
	if _, err := io.ReadFull(r, numSecTypes[:]); err != nil {
		return fmt.Errorf("reading security types count: %w", err)
	}
	if numSecTypes[0] == 0 {
		// Connection failed — server sends a reason string.
		return fmt.Errorf("server rejected connection (0 security types)")
	}
	secTypes := make([]byte, numSecTypes[0])
	if _, err := io.ReadFull(r, secTypes); err != nil {
		return fmt.Errorf("reading security types: %w", err)
	}

	// Select security type 1 (None).
	if !slices.Contains(secTypes, byte(1)) {
		return fmt.Errorf("server does not support security type None (available: %v)", secTypes)
	}
	if err := c.ws.WriteMessage(websocket.BinaryMessage, []byte{1}); err != nil {
		return fmt.Errorf("selecting security type: %w", err)
	}

	// --- SecurityResult ---
	var secResult [4]byte
	if _, err := io.ReadFull(r, secResult[:]); err != nil {
		return fmt.Errorf("reading security result: %w", err)
	}
	if binary.BigEndian.Uint32(secResult[:]) != 0 {
		return fmt.Errorf("security handshake failed (result=%d)", binary.BigEndian.Uint32(secResult[:]))
	}

	// --- ClientInit ---
	// shared-flag = 1 (allow shared sessions).
	if err := c.ws.WriteMessage(websocket.BinaryMessage, []byte{1}); err != nil {
		return fmt.Errorf("sending ClientInit: %w", err)
	}

	// --- ServerInit ---
	// Read the 24-byte fixed header + variable-length name.
	var serverInit [24]byte
	if _, err := io.ReadFull(r, serverInit[:]); err != nil {
		return fmt.Errorf("reading ServerInit: %w", err)
	}
	nameLen := binary.BigEndian.Uint32(serverInit[20:24])
	if nameLen > 0 {
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(r, name); err != nil {
			return fmt.Errorf("reading server name: %w", err)
		}
	}

	return nil
}

// vncWebSocketURL builds the WebSocket URL for a KubeVirt VNC subresource.
func vncWebSocketURL(restConfig *rest.Config, namespace, vmiName string) (string, error) {
	host := restConfig.Host
	if host == "" {
		return "", fmt.Errorf("restConfig.Host is empty")
	}

	// Parse the host to get scheme and address.
	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("parsing host: %w", err)
	}

	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}

	path := fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachineinstances/%s/vnc",
		namespace, vmiName)

	wsURL := fmt.Sprintf("%s://%s%s", scheme, u.Host, path)
	return wsURL, nil
}

// wsReader wraps a websocket.Conn to implement io.Reader by reading binary
// messages and concatenating their payloads.
type wsReader struct {
	ws  *websocket.Conn
	buf *bytes.Reader
}

func (r *wsReader) Read(p []byte) (int, error) {
	for {
		if r.buf != nil {
			n, err := r.buf.Read(p)
			if n > 0 || err != io.EOF {
				return n, err
			}
			r.buf = nil
		}
		_, data, err := r.ws.ReadMessage()
		if err != nil {
			return 0, err
		}
		r.buf = bytes.NewReader(data)
	}
}
