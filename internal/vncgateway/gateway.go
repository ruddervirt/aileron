// Package vncgateway is the open-source aileron core for VM console viewing: a
// single cluster-internal WebSocket listener that gives browsers and the build
// coordinator a shared, always-available view of a VM's console. It fronts
// guacd (which re-encodes VNC as compressed PNG/JPEG) and an in-process
// vncbridge (which tunnels guacd's plain TCP to the KubeVirt VNC WebSocket
// subresource and tracks guest resolution changes).
//
// KubeVirt allows exactly ONE VNC connection per VMI (a new one kicks the old),
// so the first client creates the guacd connection and every later client JOINS
// it. The service is deliberately authentication-free and must only be exposed
// inside the cluster; external authenticated access is layered on top. See
// docs/vncgateway.md for the full architecture and the reasoning behind the
// quirks.
package vncgateway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Bridge is the subset of *vncbridge.Bridge the gateway uses: it hands out
// localhost TCP ports tunnelling to a VMI's KubeVirt VNC console and probes
// reachability. An interface so tests can supply a fake.
type Bridge interface {
	EnsureTunnel(ctx context.Context, namespace, vmi string) (int, error)
	Probe(ctx context.Context, namespace, vmi string) error
}

// Config is the gateway's runtime configuration. ConnectSettings are the static
// guacd VNC-leg parameters keyed by guacd parameter name (hostname and port are
// added per session); JoinSettings apply to joiners.
type Config struct {
	ListenAddr      string
	GuacdAddr       string
	ConsoleWait     time.Duration
	ProbeInterval   time.Duration
	ConnectSettings map[string]string
	JoinSettings    map[string]string
}

// BuildConfig assembles the configuration from environment variables, with the
// same defaults the former Node gateway used (see docs/vncgateway.md). getenv is
// injected for testability (pass os.Getenv in production).
func BuildConfig(getenv func(string) string) Config {
	def := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	ms := func(key string, fallback int) time.Duration {
		if v := getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return time.Duration(n) * time.Millisecond
			}
		}
		return time.Duration(fallback) * time.Millisecond
	}

	connect := map[string]string{
		"autoretry":      def("GUAC_AUTORETRY", "3"),
		"color-depth":    def("GUAC_COLOR_DEPTH", "24"),
		"encodings":      def("GUAC_ENCODINGS", "zrle copyrect"),
		"quality-level":  def("GUAC_QUALITY_LEVEL", "8"),
		"compress-level": def("GUAC_COMPRESS_LEVEL", "2"),
		"cursor":         def("GUAC_CURSOR", "remote"),
	}
	// guacd 1.6.0's client-driven resize wipes the initial frame; QEMU ignores
	// client-driven resize anyway, so this is harmless on 1.5.5. Default on
	// unless explicitly set to "false".
	const valTrue = "true"
	if getenv("GUAC_DISABLE_DISPLAY_RESIZE") != "false" {
		connect["disable-display-resize"] = valTrue
	}
	if getenv("GUAC_FORCE_LOSSLESS") == valTrue {
		connect["force-lossless"] = valTrue
	}

	return Config{
		ListenAddr:      ":" + def("LISTEN_PORT", "7778"),
		GuacdAddr:       net.JoinHostPort(def("GUACD_HOST", "127.0.0.1"), def("GUACD_PORT", "4822")),
		ConsoleWait:     ms("CONSOLE_WAIT_MS", 10*60*1000),
		ProbeInterval:   ms("PROBE_INTERVAL_MS", 1000),
		ConnectSettings: connect,
		JoinSettings:    map[string]string{"read-only": "false"},
	}
}

// Gateway is the HTTP/WebSocket server. Build it with New and serve Handler.
type Gateway struct {
	cfg      Config
	bridge   Bridge
	registry *sessionRegistry
	log      *slog.Logger
}

// New builds a Gateway. bridge is the in-process vncbridge; log defaults to the
// slog default logger if nil.
func New(cfg Config, bridge Bridge, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{
		cfg:      cfg,
		bridge:   bridge,
		registry: newRegistry(bridge.EnsureTunnel, log),
		log:      log,
	}
}

// ListenAndServe runs the gateway on cfg.ListenAddr until the server errors.
func (g *Gateway) ListenAndServe() error {
	srv := &http.Server{
		Addr:    g.cfg.ListenAddr,
		Handler: g.Handler(),
	}
	g.log.Info("vncgateway listening", "addr", g.cfg.ListenAddr)
	return srv.ListenAndServe()
}
