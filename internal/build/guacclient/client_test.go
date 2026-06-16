package guacclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeGateway upgrades to WebSocket, immediately sends a first instruction
// (like guacamole-lite after guacd ready), then records received instructions
// and periodically sends sync pings.
func fakeGateway(t *testing.T, received chan<- *Instruction) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()

		// First server instruction: a size update, then a sync ping.
		_ = ws.WriteMessage(websocket.TextMessage, Encode("size", "0", "1024", "768"))
		_ = ws.WriteMessage(websocket.TextMessage, Encode("sync", "42"))

		var dec Decoder
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			dec.Feed(data)
			for {
				ins, err := dec.Next()
				if err != nil || ins == nil {
					break
				}
				if ins.Opcode == "quit" {
					return // simulate the gateway dropping the connection
				}
				received <- ins
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestDialReadyAndSyncEcho(t *testing.T) {
	received := make(chan *Instruction, 16)
	srv := fakeGateway(t, received)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(srv))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	// The client must echo the sync ping with the same timestamp.
	select {
	case ins := <-received:
		if ins.Opcode != "sync" || len(ins.Args) != 1 || ins.Args[0] != "42" {
			t.Fatalf("expected sync echo with ts 42, got %+v", ins)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no sync echo received")
	}
}

func TestSendKey(t *testing.T) {
	received := make(chan *Instruction, 16)
	srv := fakeGateway(t, received)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(srv))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.SendKey(0xff0d, true); err != nil {
		t.Fatalf("SendKey: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ins := <-received:
			if ins.Opcode == "sync" {
				continue // echo of the ping
			}
			if ins.Opcode != "key" || len(ins.Args) != 2 || ins.Args[0] != "65293" || ins.Args[1] != "1" {
				t.Fatalf("expected key 65293 down, got %+v", ins)
			}
			return
		case <-deadline:
			t.Fatal("key instruction not received")
		}
	}
}

func TestDialFailsWhenClosedBeforeDisplayInstruction(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Pre-VNC control traffic, exactly as observed live: connection-ID
		// (empty opcode), then nop. A connection that dies after only this
		// must still count as not-up-yet.
		_ = ws.WriteMessage(websocket.TextMessage, Encode("", "$abc"))
		_ = ws.WriteMessage(websocket.TextMessage, Encode("ready", "$abc"))
		_ = ws.WriteMessage(websocket.TextMessage, Encode("nop"))
		_ = ws.Close()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, wsURL(srv)); err == nil {
		t.Fatal("expected Dial to fail when server closes before any display instruction")
	}
}

func TestSendKeyFailsAfterClose(t *testing.T) {
	received := make(chan *Instruction, 16)
	srv := fakeGateway(t, received)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(srv))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Ask the fake gateway to drop the connection (httptest's
	// CloseClientConnections does not cover hijacked websocket conns).
	if err := c.write(Encode("quit")); err != nil {
		t.Fatalf("write quit: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.SendKey(0x41, true); err != nil {
			return // expected failure surfaced
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("SendKey kept succeeding after connection loss")
}
