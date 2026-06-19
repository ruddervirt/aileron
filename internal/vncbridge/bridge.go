package vncbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/rest"
)

// dns1123 mirrors what Kubernetes enforces for resource names. namespace and
// vmi land verbatim in an upstream URL path; accepting anything else could
// smuggle `..`, `?`, `#`, or other control characters into the dial.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validName(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	return dns1123.MatchString(s)
}

type dialFunc func(ctx context.Context, namespace, vmi string) (*websocket.Conn, error)

// Bridge exposes per-VMI localhost TCP listeners that pipe raw RFB bytes to
// the KubeVirt VNC WebSocket subresource. It exists because guacd only
// speaks plain TCP while KubeVirt's VNC is reachable only as a Kubernetes
// API websocket. The gateway requests listeners in-process (EnsureTunnel);
// guacd then dials the returned localhost port.
//
// Beyond plumbing, the bridge is also where guest resolution changes are
// detected (see rfbstream.go) — it is the only component that sees the raw
// RFB stream, and KubeVirt's one-connection-per-VMI rule forbids watching
// out-of-band. See docs/vncgateway.md for the architecture.
//
// The gateway (internal/vncgateway) uses this in-process via EnsureTunnel and
// Probe; the HTTP Handler below is retained for the bridge's own tests.
type Bridge struct {
	dial        dialFunc
	idleTimeout time.Duration

	mu      sync.Mutex
	closed  bool
	tunnels map[string]*tunnel // key "namespace/vmi"
}

// New builds a Bridge that dials KubeVirt with the given rest config.
func New(restCfg *rest.Config, idleTimeout time.Duration) *Bridge {
	return newWithDialer(func(ctx context.Context, namespace, vmi string) (*websocket.Conn, error) {
		return dialKubeVirtVNC(ctx, restCfg, namespace, vmi)
	}, idleTimeout)
}

func newWithDialer(dial dialFunc, idleTimeout time.Duration) *Bridge {
	return &Bridge{
		dial:        dial,
		idleTimeout: idleTimeout,
		tunnels:     make(map[string]*tunnel),
	}
}

// EnsureTunnel returns the localhost TCP port for the given VMI, creating the
// listener on first use. Idempotent: repeat calls return the same port while
// the tunnel is alive.
func (b *Bridge) EnsureTunnel(_ context.Context, namespace, vmi string) (int, error) {
	key := namespace + "/" + vmi

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, errBridgeClosed
	}
	if t, ok := b.tunnels[key]; ok {
		return t.port(), nil
	}

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen for tunnel %s: %w", key, err)
	}

	t := &tunnel{
		key:       key,
		namespace: namespace,
		vmi:       vmi,
		ln:        ln,
		bridge:    b,
	}
	t.idleTimer = time.AfterFunc(b.idleTimeout, t.reap)
	b.tunnels[key] = t
	go t.acceptLoop()

	slog.Info("tunnel created", "tunnel", key, "port", t.port())
	return t.port(), nil
}

// Probe checks whether the VMI's VNC console is reachable right now by
// dialing the KubeVirt subresource and immediately closing. Used by the
// gateway to hold client connections until the console exists instead of
// churning doomed guacd connections.
//
// WARNING: KubeVirt allows only ONE VNC connection per VMI — probing while a
// session is ACTIVE kicks that session. The gateway upholds this by only
// probing when its registry has no session for the VM (waitForConsole in
// internal/vncgateway). Do not add new Probe callers without preserving that
// invariant.
func (b *Bridge) Probe(ctx context.Context, namespace, vmi string) error {
	ws, err := b.dial(ctx, namespace, vmi)
	if err != nil {
		return err
	}
	_ = ws.Close()
	return nil
}

// CloseTunnel tears down the tunnel for the given VMI if one exists.
func (b *Bridge) CloseTunnel(namespace, vmi string) {
	key := namespace + "/" + vmi
	b.mu.Lock()
	t, ok := b.tunnels[key]
	if ok {
		delete(b.tunnels, key)
	}
	b.mu.Unlock()
	if ok {
		t.close()
	}
}

// Close tears down all tunnels.
func (b *Bridge) Close() {
	b.mu.Lock()
	b.closed = true
	tunnels := make([]*tunnel, 0, len(b.tunnels))
	for _, t := range b.tunnels {
		tunnels = append(tunnels, t)
	}
	b.tunnels = make(map[string]*tunnel)
	b.mu.Unlock()
	for _, t := range tunnels {
		t.close()
	}
}

func (b *Bridge) remove(t *tunnel) {
	b.mu.Lock()
	if cur, ok := b.tunnels[t.key]; ok && cur == t {
		delete(b.tunnels, t.key)
	}
	b.mu.Unlock()
}

type tunnelRequest struct {
	Namespace string `json:"namespace"`
	VMI       string `json:"vmi"`
}

type tunnelResponse struct {
	Port int `json:"port"`
}

// Handler serves the localhost tunnel API:
//
//	POST   /tunnels                {"namespace","vmi"} -> {"port":N}
//	DELETE /tunnels/{namespace}/{vmi}
//	GET    /readyz
func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/tunnels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req tunnelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if !validName(req.Namespace) || !validName(req.VMI) {
			http.Error(w, "invalid namespace or vmi name", http.StatusBadRequest)
			return
		}
		port, err := b.EnsureTunnel(r.Context(), req.Namespace, req.VMI)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tunnelResponse{Port: port})
	})

	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req tunnelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if !validName(req.Namespace) || !validName(req.VMI) {
			http.Error(w, "invalid namespace or vmi name", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := b.Probe(ctx, req.Namespace, req.VMI); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/tunnels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/tunnels/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
			http.Error(w, "expected /tunnels/{namespace}/{vmi}", http.StatusBadRequest)
			return
		}
		b.CloseTunnel(parts[0], parts[1])
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}
