package vncgateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// sessionRegistry makes multi-client muxing transparent: the first client for a
// VM creates the guacd connection (CONNECT), every later client JOINS it via
// the Guacamole connection-join feature. KubeVirt allows exactly ONE VNC
// connection per VMI (a new connection kicks the existing one), so all viewers
// MUST share a single upstream connection.
//
// Keyed by "namespace/vmi". Single-replica, in-memory by design — see
// docs/vncgateway.md. This is the Go port of the former vncgateway/lib/registry.js;
// JS promises become a per-session `ready` channel guarded by settleOnce.
type sessionRegistry struct {
	// ensureTunnel asks the in-process bridge for a localhost TCP port piping
	// to the VMI's KubeVirt VNC console.
	ensureTunnel func(ctx context.Context, namespace, vmi string) (int, error)
	log          *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
}

// session is one shared guacd connection and its members. clients counts every
// member that reached guacd 'ready' (primary and joiners alike): the session
// lives while any member remains, so the primary leaving with joiners still
// attached keeps it alive.
type session struct {
	key      string
	clients  int
	connID   string // guacd "$..." id; set when the primary opens
	readyErr error
	ready    chan struct{} // closed (once) when connID is set or the session fails
	once     sync.Once
}

// settle records the open outcome and unblocks any joiners waiting on ready.
// Must be called with the registry lock held (it writes connID/readyErr, which
// Acquire reads under the lock).
func (s *session) settle(connID string, err error) {
	s.once.Do(func() {
		s.connID = connID
		s.readyErr = err
		close(s.ready)
	})
}

// acquireResult tells the serve loop how to dial guacd: a fresh CONNECT for the
// primary (with the bridge tunnel port) or a JOIN onto the primary's connID.
type acquireResult struct {
	primary bool
	port    int
	connID  string
}

func newRegistry(ensureTunnel func(ctx context.Context, namespace, vmi string) (int, error), log *slog.Logger) *sessionRegistry {
	return &sessionRegistry{
		ensureTunnel: ensureTunnel,
		log:          log,
		sessions:     make(map[string]*session),
	}
}

// Has reports whether a session (live or pending) exists for ns/vmi. The serve
// loop uses this to gate probing: probing kicks any active VNC session, so it
// must only happen when there is provably nothing to kick.
func (r *sessionRegistry) Has(namespace, vmi string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sessions[namespace+"/"+vmi]
	return ok
}

func (r *sessionRegistry) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Acquire returns how to attach a client to ns/vmi: a CONNECT for the first
// client (it creates the session and ensures the bridge tunnel), a JOIN once
// the primary has opened. It blocks joiners until the primary opens or fails.
func (r *sessionRegistry) Acquire(ctx context.Context, namespace, vmi string) (acquireResult, error) {
	key := namespace + "/" + vmi

	r.mu.Lock()
	s := r.sessions[key]
	if s == nil {
		s = &session{key: key, ready: make(chan struct{})}
		r.sessions[key] = s
		r.mu.Unlock()

		port, err := r.ensureTunnel(ctx, namespace, vmi)
		if err != nil {
			r.mu.Lock()
			if r.sessions[key] == s {
				delete(r.sessions, key)
			}
			s.settle("", err) // fail any joiner that began waiting
			r.mu.Unlock()
			return acquireResult{}, err
		}
		r.log.Info("session: new connection", "vm", key, "port", port)
		return acquireResult{primary: true, port: port}, nil
	}
	r.mu.Unlock()

	select {
	case <-s.ready:
		r.mu.Lock()
		connID, rerr := s.connID, s.readyErr
		r.mu.Unlock()
		if rerr != nil {
			return acquireResult{}, rerr
		}
		r.log.Info("session: joining", "vm", key, "conn", connID)
		return acquireResult{connID: connID}, nil
	case <-ctx.Done():
		return acquireResult{}, ctx.Err()
	}
}

// PrimaryOpened records that the primary reached guacd 'ready': it sets the
// connID joiners need and counts the primary as a member.
func (r *sessionRegistry) PrimaryOpened(key, connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[key]
	if s == nil {
		return
	}
	s.clients++
	s.settle(connID, nil)
	r.log.Info("session: open", "vm", key, "conn", connID)
}

// JoinOpened records that a joiner reached guacd 'ready'.
func (r *sessionRegistry) JoinOpened(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[key]; s != nil {
		s.clients++
	}
}

// AttachFailed handles a client whose guacd handshake failed before it opened.
// If the primary failed, the session can never open: drop it and fail any
// waiters so they retry as a fresh CONNECT. A joiner failing leaves the session
// intact for the others.
func (r *sessionRegistry) AttachFailed(key string, primary bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[key]
	if s == nil {
		return
	}
	if primary && s.connID == "" {
		delete(r.sessions, key)
		if err == nil {
			err = errors.New("primary connection failed before ready")
		}
		s.settle("", err)
		r.log.Info("session: dropped before open", "vm", key, "error", err)
	}
}

// Closed handles a member whose relay ended. A guacd error is connection-scoped
// (the whole guacd connection, and thus session.connID, is dead): drop the
// entry immediately so retrying clients get a fresh connection instead of
// joining a corpse. Otherwise the session lives until its last member leaves.
func (r *sessionRegistry) Closed(key string, guacdErr bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[key]
	if s == nil {
		return
	}
	if guacdErr {
		delete(r.sessions, key)
		s.settle("", errors.New("guacd connection error"))
		r.log.Info("session: dropped on guacd error", "vm", key)
		return
	}
	s.clients--
	if s.clients <= 0 {
		delete(r.sessions, key)
		r.log.Info("session: last client left", "vm", key)
	}
}
