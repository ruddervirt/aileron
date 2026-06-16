package ui

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
)

// vncUpgrader upgrades incoming browser connections. Origin checks are not
// enforced: aileron-ui is unauthenticated and meant for trusted networks only,
// and the browser connects to its own origin anyway.
var vncUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// proxyVNC reverse-proxies a browser WebSocket to the cluster-internal
// vncgateway listener (ws://.../internal/{ns}/{vmi}?reattach=1). The gateway is
// unauthenticated and ClusterIP-only; this proxy is the in-cluster hop that
// makes a console reachable from the UI's own origin without a JWT.
//
// reattach=1 keeps the browser's socket open across session deaths (transparent
// re-attach), which is what human viewers want.
func (s *Server) proxyVNC(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	vmi := r.PathValue("vmi")
	if ns == "" || vmi == "" {
		http.Error(w, "namespace and vmi are required", http.StatusBadRequest)
		return
	}

	upstreamURL := fmt.Sprintf("%s/internal/%s/%s?reattach=1",
		s.gatewayURL, url.PathEscape(ns), url.PathEscape(vmi))

	// Dial upstream before upgrading the client so a gateway failure surfaces
	// as a plain HTTP error instead of a half-open WebSocket.
	upstream, _, err := websocket.DefaultDialer.DialContext(r.Context(), upstreamURL, nil)
	if err != nil {
		s.log.Error("vnc upstream dial failed", "url", upstreamURL, "error", err)
		http.Error(w, "cannot reach vnc gateway", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	browser, err := vncUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		s.log.Warn("vnc client upgrade failed", "error", err)
		return
	}
	defer func() { _ = browser.Close() }()

	s.log.Info("vnc session opened", "namespace", ns, "vmi", vmi)

	// Pump both directions; the first side to error/close ends the session and
	// the deferred Close calls unblock the other goroutine. errc is buffered so
	// neither send blocks once we stop receiving.
	errc := make(chan error, 2)
	go pumpWS(browser, upstream, errc)
	go pumpWS(upstream, browser, errc)
	<-errc

	s.log.Info("vnc session closed", "namespace", ns, "vmi", vmi)
}

// pumpWS copies messages from src to dst, preserving the WebSocket message type
// (the guacamole protocol rides on TEXT frames, so type must not be coerced).
func pumpWS(dst, src *websocket.Conn, errc chan<- error) {
	for {
		mt, data, err := src.ReadMessage()
		if err != nil {
			errc <- err
			return
		}
		if err := dst.WriteMessage(mt, data); err != nil {
			errc <- err
			return
		}
	}
}
