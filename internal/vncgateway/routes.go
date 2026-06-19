package vncgateway

import (
	_ "embed"
	"net/http"

	"github.com/gorilla/websocket"
)

// debugHTML is the built-in guacamole-common-js viewer served at
// /internal/debug/{ns}/{vmi} (opened by hack/vncview.sh).
//
//go:embed debug.html
var debugHTML []byte

const (
	internalPrefix      = "/internal/"
	internalDebugPrefix = "/internal/debug/"
)

// upgrader offers the "guacamole" subprotocol the coordinator client requests.
// Origin checks are not enforced: the gateway is unauthenticated and
// cluster-internal by design (external authenticated access is layered on top).
var upgrader = websocket.Upgrader{
	Subprotocols: []string{"guacamole"},
	CheckOrigin:  func(*http.Request) bool { return true },
}

// Handler builds the gateway's HTTP routes:
//
//	GET /healthz                       -> "ok"
//	GET /internal/debug/{ns}/{vmi}     -> debug viewer HTML
//	    /internal/{ns}/{vmi}[?reattach=1] -> WebSocket (Guacamole protocol)
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Longest-prefix wins, so debug paths never reach handleInternal.
	mux.HandleFunc(internalDebugPrefix, g.handleDebug)
	mux.HandleFunc(internalPrefix, g.handleInternal)
	return mux
}

// handleDebug serves the embedded viewer. WebSocket upgrades on the debug
// prefix are rejected (the viewer connects to the /internal/{ns}/{vmi} path).
func (g *Gateway) handleDebug(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, _, ok := parseTwoSegments(r.URL.Path, internalDebugPrefix); !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write(debugHTML)
}

// handleInternal upgrades the WebSocket and hands it to serve. The upgrade
// completes immediately (even before the console exists); serve then holds the
// socket with nop keepalives until it can attach.
func (g *Gateway) handleInternal(w http.ResponseWriter, r *http.Request) {
	ns, vmi, ok := parseTwoSegments(r.URL.Path, internalPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Default is fail-fast (the coordinator must see session death immediately
	// so keys are never typed into a void); human viewers (and the auth relay
	// on their behalf) opt into transparent re-attach with ?reattach=1.
	reattach := r.URL.Query().Get("reattach") == "1"

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		return
	}
	g.serve(ws, ns, vmi, reattach)
}
