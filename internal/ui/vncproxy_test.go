package ui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestProxyVNC verifies the reverse proxy reaches the gateway at the expected
// /internal path with reattach=1 and relays messages both directions,
// preserving the (text) message type the guacamole protocol depends on.
func TestProxyVNC(t *testing.T) {
	gotPath := make(chan string, 1)
	gotReattach := make(chan string, 1)
	upstreamGot := make(chan string, 1)

	upstreamUpgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		gotReattach <- r.URL.Query().Get("reattach")
		conn, err := upstreamUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		// Echo what the browser sent, then push a server-originated frame.
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			t.Errorf("upstream got message type %d, want text", mt)
		}
		upstreamGot <- string(data)
		_ = conn.WriteMessage(websocket.TextMessage, []byte("from-gateway"))
		// Hold open briefly so the client read lands before teardown.
		time.Sleep(100 * time.Millisecond)
	}))
	defer upstream.Close()

	gatewayURL := "ws://" + strings.TrimPrefix(upstream.URL, "http://")
	srv := NewServer(nil, nil, "ruddervirt-system", gatewayURL,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	clientURL := "ws://" + strings.TrimPrefix(proxy.URL, "http://") + "/vnc/test-ns/test-vmi"
	client, _, err := websocket.DefaultDialer.Dial(clientURL, nil)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.WriteMessage(websocket.TextMessage, []byte("from-browser")); err != nil {
		t.Fatalf("write to proxy: %v", err)
	}

	if got := <-gotPath; got != "/internal/test-ns/test-vmi" {
		t.Errorf("upstream path = %q, want /internal/test-ns/test-vmi", got)
	}
	if got := <-gotReattach; got != "1" {
		t.Errorf("upstream reattach = %q, want 1", got)
	}
	if got := <-upstreamGot; got != "from-browser" {
		t.Errorf("upstream received %q, want from-browser", got)
	}

	mt, data, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read from proxy: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Errorf("client got message type %d, want text", mt)
	}
	if string(data) != "from-gateway" {
		t.Errorf("client received %q, want from-gateway", string(data))
	}
}

// TestProxyVNCUpstreamUnreachable verifies a dial failure surfaces as a 502
// before the client WebSocket is upgraded.
func TestProxyVNCUpstreamUnreachable(t *testing.T) {
	srv := NewServer(nil, nil, "ruddervirt-system", "ws://127.0.0.1:1",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	clientURL := "ws://" + strings.TrimPrefix(proxy.URL, "http://") + "/vnc/ns/vmi"
	_, resp, err := websocket.DefaultDialer.Dial(clientURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail when upstream is unreachable")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502 Bad Gateway, got %v", resp)
	}
}
