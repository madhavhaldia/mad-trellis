package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// CallContext carries per-call context to a Handler. Session is the
// daemon-minted, connection-bound identity (Inv 4): handlers MUST use this and
// MUST NOT trust any identity supplied in params.
type CallContext struct {
	Session SessionID
	Daemon  *Daemon
}

// RebindSession is the SINGLE sanctioned mutation of cc.Session after
// Authenticate. It is the surgically-guarded Inv 4 exception: a connection's
// identity is established by the Authenticator on accept (per-connection minting,
// unchanged) and is otherwise immutable — handlers never write cc.Session. The
// SOLE caller is the session.attach handler, which rebinds this connection to an
// already-existing, still-LIVE session named ONLY indirectly via an unforgeable,
// daemon-minted capability token (never a client-supplied session id). Do NOT
// call this from any other path: a direct, token-less rebind would let a client
// assume an arbitrary identity, violating the unspoofable-identity invariant.
func (cc *CallContext) RebindSession(id SessionID) { cc.Session = id }

// Handler implements one registered method. It must be deterministic with
// respect to (cc.Session, params): no probabilistic component anywhere on a
// path that could decide exclusive access (Inv 2(b)).
type Handler func(cc *CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error)

// Registry maps method names to handlers. The daemon dispatches by name without
// knowing a method's semantics (Inv 10-decoupling): downstream projects
// register their own methods. The registry is FROZEN before fan-out; changing
// the surface afterward requires re-review.
type Registry struct {
	mu      sync.RWMutex
	methods map[string]Handler
	frozen  bool
}

func newRegistry() *Registry { return &Registry{methods: map[string]Handler{}} }

// Register adds a method. It errors on a duplicate name and after the registry
// is frozen.
func (r *Registry) Register(method string, h Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("registry frozen: cannot register %q", method)
	}
	if _, dup := r.methods[method]; dup {
		return fmt.Errorf("duplicate method %q", method)
	}
	r.methods[method] = h
	return nil
}

// RegisterStub registers a method that returns the canonical not-implemented
// error — used to publish a frozen method signature before its body exists.
func (r *Registry) RegisterStub(method string) error {
	return r.Register(method, func(*CallContext, json.RawMessage) (json.RawMessage, *protocol.Error) {
		return nil, protocol.ErrNotImplemented
	})
}

func (r *Registry) lookup(method string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.methods[method]
	return h, ok
}

// Freeze locks the registry against further registration.
func (r *Registry) Freeze() {
	r.mu.Lock()
	r.frozen = true
	r.mu.Unlock()
}

// Methods returns the registered method names, sorted.
func (r *Registry) Methods() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.methods))
	for k := range r.methods {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
