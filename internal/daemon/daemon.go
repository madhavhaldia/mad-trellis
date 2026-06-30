// Package daemon implements project 1 (daemon-arbiter-protocol): the single
// long-lived headless arbiter process and the frozen Unix-socket JSON-RPC
// contract every client codes against.
//
// Owns (docs/0003 clause map): 5-arbiter (a PROCESS singleton, not only a
// ledger singleton), 10-decoupling (no agent/host/orchestrator coupling; MCP
// dialect kept out of this API), and its local 2(b) slice (the dispatch→authz→
// routing path is deterministic, no probabilistic component). It hosts the
// decision-audit WRITE-INTERFACE + record contract (storage is the lease
// ledger's job) and op-diagnostics. It holds NO governance policy.
package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/protocol"
)

// ErrAlreadyRunning is returned when a LIVE daemon already owns the socket. The
// single-arbiter guarantee (Inv 5) is enforced at the process level: a second
// instance refuses rather than taking over.
var ErrAlreadyRunning = errors.New("mad-substrate daemon already running on socket")

// Options configures a Daemon.
type Options struct {
	SocketPath string        // required: the Unix socket path
	Auth       Authenticator // optional: defaults to per-connection minted identity
	Audit      AuditSink     // optional: defaults to NoopSink
	// AuditReader is the OPTIONAL read-only tail seam for the watch-view surface
	// (project 9a). When nil, audit.tail still serves but returns an EMPTY record
	// list (never an error), so a minimally-composed daemon stays serviceable.
	AuditReader AuditReader
}

// Daemon is the single arbiter process. Construct with New, bind with Start,
// then Serve.
type Daemon struct {
	socketPath string
	ln         net.Listener
	lockFile   *os.File
	registry   *Registry
	auth       Authenticator
	audit      AuditSink
	auditRead  AuditReader // optional read-only tail seam (nil => empty tail)

	startTime   time.Time
	pid         int
	activeConns atomic.Int64

	// maxConns caps the number of in-flight handleConn goroutines (and thus held
	// fds). connSem is the bounding semaphore, sized to maxConns at Start. See
	// Serve for the availability rationale (a slow-loris connection that never
	// speaks otherwise parks a goroutine+fd forever).
	maxConns int
	connSem  chan struct{}

	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// New constructs a Daemon. It does not bind the socket (use Start).
func New(opts Options) *Daemon {
	if opts.Auth == nil {
		opts.Auth = &mintedAuth{}
	}
	if opts.Audit == nil {
		opts.Audit = NoopSink{}
	}
	d := &Daemon{
		socketPath: opts.SocketPath,
		registry:   newRegistry(),
		auth:       opts.Auth,
		audit:      opts.Audit,
		auditRead:  opts.AuditReader, // nil by default => audit.tail serves an empty list
		maxConns:   defaultMaxConns,
		startTime:  time.Now(),
		pid:        os.Getpid(),
	}
	d.registerBuiltins()
	return d
}

// Registry exposes the method registry for downstream registration.
func (d *Daemon) Registry() *Registry { return d.registry }

// SocketPath returns the bound socket path.
func (d *Daemon) SocketPath() string { return d.socketPath }

// Start acquires the socket as the single daemon instance (Inv 5-arbiter) and
// writes the authoritative pidfile (this daemon's OWN os.Getpid(), recorded
// out-of-band of any RPC self-report) so `daemon stop` can signal the true
// owner. The pidfile is removed by Close.
func (d *Daemon) Start() error {
	h, err := acquireSocket(d.socketPath)
	if err != nil {
		return err
	}
	d.ln = h.ln
	d.lockFile = h.lockFile
	// Size the concurrent-connection semaphore now that we are the bound,
	// single instance. A test may set a smaller maxConns before Start to drive
	// the cap deterministically; guard against a non-positive value.
	if d.maxConns <= 0 {
		d.maxConns = defaultMaxConns
	}
	d.connSem = make(chan struct{}, d.maxConns)
	// Record the authoritative pid next to the lock. Best-effort: a daemon that
	// could bind the socket and hold the flock is fully serviceable even if the
	// pidfile write fails (stop falls back to the socket-refuses cross-check), so
	// we never fail Start on it — but a successful write is the common path.
	_ = writePidfile(PidfilePath(d.socketPath), d.pid)
	return nil
}

// defaultMaxConns bounds the number of in-flight handleConn goroutines (and thus
// the fds they hold). It is generous enough to never throttle legitimate use —
// the launcher holds ONE long-lived keepalive connection per governed session —
// while capping the blast radius of a connection that connects and then never
// speaks (a relay-opened slow-loris would otherwise park a goroutine+fd forever,
// since the Decode loop has no idle deadline). This is an AVAILABILITY bound, not
// an admission-control policy; identity authz is unchanged.
const defaultMaxConns = 256

// Serve accepts connections until Close, bounding in-flight handleConn goroutines
// with connSem. A slot is taken BEFORE Accept so a saturated daemon stops pulling
// new connections off the listener backlog (rather than accepting fds it cannot
// service); each accepted connection's goroutine releases its slot on exit. This
// does not touch the read path, so a legitimate idle-but-alive keepalive
// connection is never severed — it simply occupies one of the maxConns slots.
func (d *Daemon) Serve() error {
	for {
		d.connSem <- struct{}{}
		conn, err := d.ln.Accept()
		if err != nil {
			<-d.connSem
			if d.isClosed() {
				return nil
			}
			return err
		}
		go func() {
			defer func() { <-d.connSem }()
			d.handleConn(conn)
		}()
	}
}

// Close stops serving and unlinks the socket. (For P0 there are no leases yet;
// once the lease ledger exists, graceful shutdown must strand no lock.)
func (d *Daemon) Close() error {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
		if d.ln != nil {
			d.closeErr = d.ln.Close() // unlinks the socket file
		}
		// Remove the authoritative pidfile before releasing the flock, so the
		// window where the flock is free but a pidfile lingers is closed by the
		// daemon itself (a stale pidfile from a CRASH is still handled by readers).
		_ = os.Remove(PidfilePath(d.socketPath))
		if d.lockFile != nil {
			releaseLock(d.lockFile)
		}
	})
	return d.closeErr
}

func (d *Daemon) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	d.activeConns.Add(1)
	defer d.activeConns.Add(-1)

	// Defense-in-depth (Phase 4): reject a peer that is DEFINITIVELY a different
	// OS user than this daemon. The PRIMARY admission control remains the 0600
	// socket-file permission set in acquireSocket; this LOCAL_PEERCRED same-uid
	// check removes sole reliance on file perms for the identity trust root. It
	// is fail-OPEN: any error reading the peer uid ALLOWS the connection, so a
	// legitimate same-uid client is never broken (the trust root is hardened,
	// not narrowed).
	if peerConnMismatch(conn) {
		_ = json.NewEncoder(conn).Encode(errResponse(nil, protocol.NewError(protocol.CodeAuthz, "peer uid mismatch")))
		return
	}

	// Authz: establish the unspoofable, connection-bound identity BEFORE any
	// method body runs. An authentication failure rejects the connection.
	session, err := d.auth.Authenticate(conn)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(errResponse(nil, protocol.NewError(protocol.CodeAuthz, "authentication failed")))
		return
	}
	cc := &CallContext{Session: session, Daemon: d}

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req protocol.Request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			_ = enc.Encode(errResponse(nil, protocol.NewError(protocol.CodeParse, "parse error")))
			return
		}
		if err := enc.Encode(d.dispatch(cc, &req)); err != nil {
			return
		}
	}
}

// dispatch validates the envelope and routes to the registered handler. The
// path is deterministic (Inv 2(b)): no probabilistic component decides routing.
func (d *Daemon) dispatch(cc *CallContext, req *protocol.Request) *protocol.Response {
	if verr := req.Validate(); verr != nil {
		return errResponse(req.ID, verr)
	}
	h, ok := d.registry.lookup(req.Method)
	if !ok {
		return errResponse(req.ID, protocol.NewError(protocol.CodeMethodNotFound, "method not found: "+req.Method))
	}
	result, rerr := h(cc, req.Params)
	if rerr != nil {
		return errResponse(req.ID, rerr)
	}
	return &protocol.Response{
		JSONRPC: protocol.JSONRPCVersion,
		V:       protocol.ContractVersion,
		ID:      req.ID,
		Result:  result,
	}
}

// registerBuiltins registers the agnostic spine methods. NOTE (Inv 10): these
// names are agent/host-agnostic by construction — no MCP/agent dialect appears
// here. The positive control for the decoupling test adds an MCP-typed method
// and asserts the boundary check goes RED.
func (d *Daemon) registerBuiltins() {
	// diag.health — process/socket health ONLY; never lease/trunk state.
	must(d.registry.Register("diag.health", func(cc *CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		return mustJSON(map[string]any{
			"pid":                d.pid,
			"uptime_seconds":     int64(time.Since(d.startTime).Seconds()),
			"socket_path":        d.socketPath,
			"active_connections": d.activeConns.Load(),
			"contract_version":   protocol.ContractVersion,
		}), nil
	}))

	// session.whoami — returns the caller's daemon-minted identity. Ignores
	// params entirely (identity is connection-bound, never payload-supplied).
	must(d.registry.Register("session.whoami", func(cc *CallContext, _ json.RawMessage) (json.RawMessage, *protocol.Error) {
		return mustJSON(map[string]string{"session": string(cc.Session)}), nil
	}))

	// audit.append — write-interface to the append-only decision audit. The
	// daemon stamps the timestamp and the CONNECTION-BOUND session, ignoring
	// any session/timestamp in params (Inv 4). Storage is the sink's job.
	must(d.registry.Register("audit.append", func(cc *CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			DecisionProject string          `json:"decision_project"`
			DecisionKind    string          `json:"decision_kind"`
			Payload         json.RawMessage `json:"payload,omitempty"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &in); err != nil {
				return nil, protocol.NewError(protocol.CodeInvalidParams, "invalid audit params")
			}
		}
		if in.DecisionProject == "" || in.DecisionKind == "" {
			return nil, protocol.NewError(protocol.CodeInvalidParams, "decision_project and decision_kind required")
		}
		rec := AuditRecord{
			Timestamp:       time.Now(),
			Session:         cc.Session, // connection-bound, NOT from params
			DecisionProject: in.DecisionProject,
			DecisionKind:    in.DecisionKind,
			Payload:         in.Payload,
		}
		if err := d.audit.Append(rec); err != nil {
			return nil, protocol.NewError(protocol.CodeInternal, "audit append failed")
		}
		return mustJSON(map[string]bool{"ok": true}), nil
	}))

	// audit.tail — READ-ONLY read-interface to the append-only decision audit
	// (project 9a's watch-view surface). Symmetric to audit.append but performs
	// ZERO writes: it returns the newest-first records from the AuditReader, if
	// one is configured. If no reader is installed it returns an EMPTY list (NOT
	// an error) so a minimally-composed daemon still serves the method. A
	// non-positive or oversized limit is normalized (default/cap) here, not in
	// the storage layer, so every reader behaves identically.
	must(d.registry.Register("audit.tail", func(cc *CallContext, params json.RawMessage) (json.RawMessage, *protocol.Error) {
		var in struct {
			Limit int `json:"limit"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &in); err != nil {
				return nil, protocol.NewError(protocol.CodeInvalidParams, "invalid audit.tail params")
			}
		}
		limit := normalizeAuditLimit(in.Limit)
		out := []map[string]any{}
		if d.auditRead != nil {
			recs, err := d.auditRead.Tail(limit)
			if err != nil {
				return nil, protocol.NewError(protocol.CodeInternal, "audit tail failed")
			}
			out = make([]map[string]any, 0, len(recs))
			for _, r := range recs {
				out = append(out, map[string]any{
					"timestamp_ms":     r.Timestamp.UnixMilli(),
					"session":          string(r.Session),
					"decision_project": r.DecisionProject,
					"decision_kind":    r.DecisionKind,
					"payload":          r.Payload,
				})
			}
		}
		return mustJSON(map[string]any{"records": out}), nil
	}))
}

// auditTailDefaultLimit / auditTailMaxLimit bound the read-only audit.tail
// query: a non-positive request defaults, an oversized one is capped, so a
// client can never ask the daemon to materialize an unbounded result set.
const (
	auditTailDefaultLimit = 100
	auditTailMaxLimit     = 500
)

func normalizeAuditLimit(limit int) int {
	if limit <= 0 {
		return auditTailDefaultLimit
	}
	if limit > auditTailMaxLimit {
		return auditTailMaxLimit
	}
	return limit
}

// socketHandle bundles the bound listener with the exclusive lock file that
// guarantees single-instance ownership for the daemon's lifetime.
type socketHandle struct {
	ln       net.Listener
	lockFile *os.File
}

// acquireSocket binds the Unix socket as the single daemon instance (Inv
// 5-arbiter), using an exclusive advisory lock (flock) on a sibling ".lock"
// file as the source of truth — NOT the socket file. This avoids the classic
// bind()-creates-the-file-before-listen() race, where a concurrent acquirer
// sees the file but a connect probe is refused, wrongly "reclaims" it, and a
// second arbiter is born. A LIVE daemon holds the flock for its whole life, so:
//   - flock contended => ErrAlreadyRunning (never a false takeover of a live holder)
//   - flock acquired  => no live daemon exists; any leftover socket file is
//     stale by definition and is safely removed before we listen.
func acquireSocket(path string) (*socketHandle, error) {
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lf.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	// Sole acquirer: a live daemon would still hold its flock, so none is
	// running — any leftover socket file is stale and safe to remove.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		releaseLock(lf)
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		releaseLock(lf)
		return nil, err
	}
	_ = os.Chmod(path, 0o600) // owner-only: Unix-socket-perms authz (v1)
	return &socketHandle{ln: ln, lockFile: lf}, nil
}

func releaseLock(lf *os.File) {
	_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	_ = lf.Close()
}

func errResponse(id *json.RawMessage, e *protocol.Error) *protocol.Response {
	return &protocol.Response{JSONRPC: protocol.JSONRPCVersion, V: protocol.ContractVersion, ID: id, Error: e}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
