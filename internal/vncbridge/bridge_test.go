package vncbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// echoWSServer is an httptest server that upgrades to WebSocket and echoes
// binary frames back, standing in for the KubeVirt VNC subresource.
func echoWSServer(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		for {
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if err := ws.WriteMessage(msgType, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func echoDialer(srv *httptest.Server) dialFunc {
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return func(ctx context.Context, namespace, vmi string) (*websocket.Conn, error) {
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		return ws, err
	}
}

func TestTunnelPipesBytes(t *testing.T) {
	srv := echoWSServer(t)
	b := newWithDialer(echoDialer(srv), time.Minute)
	defer b.Close()

	port, err := b.EnsureTunnel(context.Background(), "ns", "vmi-a")
	if err != nil {
		t.Fatalf("EnsureTunnel: %v", err)
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer func() { _ = conn.Close() }()

	payload := []byte("RFB 003.008\n")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if err := readFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("echo mismatch: got %q want %q", got, payload)
	}
}

func TestTunnelIdempotentPort(t *testing.T) {
	srv := echoWSServer(t)
	b := newWithDialer(echoDialer(srv), time.Minute)
	defer b.Close()

	p1, err := b.EnsureTunnel(context.Background(), "ns", "vmi-b")
	if err != nil {
		t.Fatalf("EnsureTunnel: %v", err)
	}
	p2, err := b.EnsureTunnel(context.Background(), "ns", "vmi-b")
	if err != nil {
		t.Fatalf("EnsureTunnel repeat: %v", err)
	}
	if p1 != p2 {
		t.Errorf("expected same port, got %d and %d", p1, p2)
	}
}

func TestTunnelAllowsConcurrentConnections(t *testing.T) {
	srv := echoWSServer(t)
	b := newWithDialer(echoDialer(srv), time.Minute)
	defer b.Close()

	port, err := b.EnsureTunnel(context.Background(), "ns", "vmi-c")
	if err != nil {
		t.Fatalf("EnsureTunnel: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// The tunnel itself does not enforce the one-connection rule (recycle
	// races legitimately overlap); each accepted conn gets its own upstream
	// pipe. The GATEWAY guarantees only one logical session per VM.
	conns := make([]net.Conn, 3)
	for i := range conns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		conns[i] = c
	}

	one := make([]byte, 1)
	for i, c := range conns {
		payload := []byte{byte('a' + i)}
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("write conn %d: %v", i, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := readFull(c, one); err != nil {
			t.Fatalf("read echo conn %d: %v", i, err)
		}
		if one[0] != payload[0] {
			t.Errorf("conn %d echo mismatch: got %q want %q", i, one, payload)
		}
	}
}

func TestTunnelIdleReap(t *testing.T) {
	srv := echoWSServer(t)
	b := newWithDialer(echoDialer(srv), 100*time.Millisecond)
	defer b.Close()

	port, err := b.EnsureTunnel(context.Background(), "ns", "vmi-d")
	if err != nil {
		t.Fatalf("EnsureTunnel: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		_, alive := b.tunnels["ns/vmi-d"]
		b.mu.Unlock()
		if !alive {
			// Listener must be closed too.
			if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
				_ = conn.Close()
				t.Fatal("listener still accepting after reap")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("tunnel was not reaped")
}

func TestHandlerTunnelLifecycle(t *testing.T) {
	srv := echoWSServer(t)
	b := newWithDialer(echoDialer(srv), time.Minute)
	defer b.Close()

	api := httptest.NewServer(b.Handler())
	defer api.Close()

	body := strings.NewReader(`{"namespace":"ns","vmi":"vmi-e"}`)
	resp, err := http.Post(api.URL+"/tunnels", "application/json", body)
	if err != nil {
		t.Fatalf("POST /tunnels: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /tunnels status %d", resp.StatusCode)
	}
	var tr tunnelResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if tr.Port == 0 {
		t.Fatal("expected nonzero port")
	}

	req, _ := http.NewRequest(http.MethodDelete, api.URL+"/tunnels/ns/vmi-e", nil)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	_ = dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status %d", dresp.StatusCode)
	}

	b.mu.Lock()
	_, alive := b.tunnels["ns/vmi-e"]
	b.mu.Unlock()
	if alive {
		t.Fatal("tunnel still registered after DELETE")
	}
}

func TestHandlerRejectsInvalidNames(t *testing.T) {
	b := newWithDialer(func(context.Context, string, string) (*websocket.Conn, error) {
		return nil, fmt.Errorf("must not dial")
	}, time.Minute)
	defer b.Close()

	api := httptest.NewServer(b.Handler())
	defer api.Close()

	for _, payload := range []string{
		`{"namespace":"../etc","vmi":"x"}`,
		`{"namespace":"ns","vmi":"UPPER"}`,
		`{"namespace":"","vmi":"x"}`,
		`not json`,
	} {
		resp, err := http.Post(api.URL+"/tunnels", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %q: status %d, want 400", payload, resp.StatusCode)
		}
	}
}

func readFull(conn net.Conn, buf []byte) error {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return err
		}
	}
	return nil
}

func TestProbe(t *testing.T) {
	srv := echoWSServer(t)
	b := newWithDialer(echoDialer(srv), time.Minute)
	defer b.Close()

	api := httptest.NewServer(b.Handler())
	defer api.Close()

	resp, err := http.Post(api.URL+"/probe", "application/json",
		strings.NewReader(`{"namespace":"ns","vmi":"vmi-p"}`))
	if err != nil {
		t.Fatalf("POST /probe: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("probe status %d, want 204", resp.StatusCode)
	}
}

func TestProbeUnavailable(t *testing.T) {
	b := newWithDialer(func(context.Context, string, string) (*websocket.Conn, error) {
		return nil, fmt.Errorf("HTTP 404")
	}, time.Minute)
	defer b.Close()

	api := httptest.NewServer(b.Handler())
	defer api.Close()

	resp, err := http.Post(api.URL+"/probe", "application/json",
		strings.NewReader(`{"namespace":"ns","vmi":"vmi-p"}`))
	if err != nil {
		t.Fatalf("POST /probe: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("probe status %d, want 503", resp.StatusCode)
	}
}
