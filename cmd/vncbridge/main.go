// vncbridge exposes per-VMI localhost TCP listeners that pipe raw RFB bytes
// to the KubeVirt VNC WebSocket subresource. guacd (which only speaks plain
// TCP) connects to these listeners; the vncgateway Node app requests them via
// the localhost tunnel API.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"k8s.io/client-go/rest"

	"github.com/ruddervirt/aileron/internal/vncbridge"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("in-cluster config unavailable", "error", err)
		os.Exit(1)
	}

	idleTimeout := 5 * time.Minute
	if v := os.Getenv("TUNNEL_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			slog.Error("invalid TUNNEL_IDLE_TIMEOUT", "value", v, "error", err)
			os.Exit(1)
		}
		idleTimeout = d
	}

	bridge := vncbridge.New(cfg, idleTimeout)

	// Plain liveness/readiness only: the core gateway has no external
	// audience for richer status. (A richer public /healthz with basic auth
	// + node metadata is served by an external auth proxy in front of this
	// gateway, when one is deployed.)
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", ok)
	healthMux.HandleFunc("/readyz", ok)
	go func() {
		addr := envOrDefault("HEALTH_ADDR", ":8080")
		slog.Info("health server listening", "addr", addr)
		if err := http.ListenAndServe(addr, healthMux); err != nil {
			slog.Error("health server failed", "error", err)
			os.Exit(1)
		}
	}()

	// The tunnel API stays on localhost: it is only for the vncgateway
	// container in the same pod and must never be reachable off-node.
	addr := envOrDefault("BRIDGE_LISTEN_ADDR", "127.0.0.1:9190")
	slog.Info("vncbridge tunnel API listening", "addr", addr, "idle_timeout", idleTimeout)
	if err := http.ListenAndServe(addr, bridge.Handler()); err != nil {
		slog.Error("tunnel API server failed", "error", err)
		os.Exit(1)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
