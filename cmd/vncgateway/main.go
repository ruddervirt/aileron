// vncgateway is the open-source aileron core for VM console viewing. It is a
// single cluster-internal WebSocket listener that fronts guacd and an
// in-process vncbridge: the gateway shares one guacd connection per VM among
// all viewers (KubeVirt allows one VNC connection per VMI), holds connections
// until the console exists, and transparently re-attaches human viewers across
// guest reboots. The bridge tunnels guacd's plain TCP to the KubeVirt VNC
// WebSocket subresource and tracks guest resolution changes.
//
// This service is deliberately authentication-free and must only be exposed
// inside the cluster (ClusterIP). External/authenticated access is a separate
// concern layered on top. See docs/vncgateway.md.
package main

import (
	"log/slog"
	"os"
	"time"

	"k8s.io/client-go/rest"

	"github.com/ruddervirt/aileron/internal/vncbridge"
	"github.com/ruddervirt/aileron/internal/vncgateway"
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

	// The bridge runs in-process: EnsureTunnel/Probe are direct method calls,
	// not a localhost HTTP hop. It still opens 127.0.0.1 TCP listeners that the
	// guacd sidecar dials.
	bridge := vncbridge.New(cfg, idleTimeout)
	defer bridge.Close()

	gw := vncgateway.New(vncgateway.BuildConfig(os.Getenv), bridge, slog.Default())
	if err := gw.ListenAndServe(); err != nil {
		slog.Error("vncgateway server failed", "error", err)
		os.Exit(1)
	}
}
