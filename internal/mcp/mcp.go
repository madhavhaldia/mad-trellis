// Package mcp is the agent-facing half of mad-substrate's cooperative layer: a
// hand-rolled MCP (Model Context Protocol) server, spoken over stdio, that
// exposes mad-substrate's six cooperative tools to a coding agent. It is the
// Inv-10 "agent dialect" the daemon's frozen protocol deliberately omits — it
// couples to the daemon ONLY through internal/coopclient (a second client of
// the frozen registry), never to the wire surface directly.
//
// Two invariants shape this package and are commented at their load-bearing
// sites:
//
//   - Inv 4 (connection-bound identity): ONE Serve == ONE coopclient.Client ==
//     ONE persistent connection == ONE stable daemon-minted holder. We never
//     open a connection per tool call; the holder is read once at Dial and is
//     the identity every claim/renew/release in this session coordinates as.
//
//   - Inv 13 (fail-soft is the law): the cooperative layer must NEVER make a
//     governed session more fragile than a bare one. A daemon that is slow,
//     unreachable, or absent must never crash the agent and never fail-closed:
//     Serve starts even if Dial fails, every tool degrades to a "safe to
//     proceed" advisory on a transport error, and no tool panics out.
//
// The MCP transport is newline-delimited JSON-RPC 2.0: exactly one message per
// line. We hand-roll it (no MCP SDK dependency) over bufio to keep the binary
// cgo-free and the dependency set minimal.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/conductor"
	"github.com/madhavhaldia/mad-substrate/internal/coopclient"
	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
	"github.com/madhavhaldia/mad-substrate/internal/runtimecfg"
)

// maxLine bounds a single MCP message. Agent tool calls are tiny (a path or a
// branch name); 8 MiB is a generous ceiling that still rejects a runaway line
// instead of letting it exhaust memory.
const maxLine = 8 << 20

// minRenewInterval floors the auto-renew ticker so a tiny (or misconfigured)
// LeaseTTL can never spin the renew goroutine into a busy loop.
const minRenewInterval = time.Second

const (
	integrationEventPollInterval = 2 * time.Second
	integrationEventPollMax      = 50
)

// backend is the minimal daemon-facing surface the MCP tools need. *coopclient.Client
// satisfies it (compile-time asserted below), so Serve builds a real client
// while the per-tool handlers are unit-tested against a stub — no live daemon.
type backend interface {
	Holder() string
	Route(domain, name string) (kind, leaseKey string, err error)
	Acquire(leaseKey string, ttl time.Duration) (granted bool, holder string, fence int64, err error)
	Renew(leaseKey string, ttl time.Duration) (ok bool, err error)
	Release(leaseKey string) (ok bool, err error)
	ListHolders() ([]coopclient.Holder, error)
	Integrations() ([]coopclient.Integration, error)

	// Integration plane (builder + integrator toolsets).
	RequestIntegration(branch, title string) (id, state string, err error)
	IntegrationStatus(branch string) (found bool, id, state, feedback, merge string, err error)
	IntegrationPending() ([]coopclient.PendingIntegration, error)
	IntegrationClaim(id string) (ok bool, branch, title string, err error)
	IntegrationVerdict(id, decision, feedback, merge string) (ok bool, state string, err error)
	IntegrationEvents(branch string, max int) ([]coopclient.IntegrationEvent, error)
	LeaseInspect(key []byte) (coopclient.LeaseView, error)

	Close() error
}

// Compile-time proof that the real client satisfies the backend the handlers
// are written against (so the stub and the production type can never diverge).
var _ backend = (*coopclient.Client)(nil)

// dialFunc is the indirection that lets a transport-error re-Dial (and the
// startup Dial) be stubbed in tests. It returns a backend, never a concrete
// *Client, so a test can hand back a fresh stub on re-Dial.
type dialFunc func(cfg coopclient.Config) (backend, error)

// realDial adapts coopclient.Dial to dialFunc. The concrete *Client is wrapped
// in the backend interface; a nil client (Dial failed) surfaces as a nil
// backend so the server stays in its fail-soft "no daemon" state.
func realDial(cfg coopclient.Config) (backend, error) {
	c, err := coopclient.Dial(cfg)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Serve runs the MCP server over the given stdio streams until ctx is cancelled
// or in hits EOF. version is reported in serverInfo. logf is a best-effort
// structured logger (may be nil); it is used ONLY for diagnostics — never for
// protocol output, which goes to out.
//
// FAIL-SOFT lifecycle (Inv 13): cfg is loaded, then we best-effort Dial. If
// Dial fails we STILL serve — tools return a fail-soft note rather than the
// agent losing its tools.
func Serve(ctx context.Context, in io.Reader, out io.Writer, version, role string, logf func(format string, args ...any)) error {
	cfg := coopclient.Load()
	return serveWith(ctx, in, out, version, normalizeRole(role), logf, cfg, realDial)
}

// Role names. builder is the default agent toolset; integrator is the trunk-side
// reviewer toolset that performs the gated merge.
const (
	roleBuilder    = "builder"
	roleIntegrator = "integrator"
)

// normalizeRole maps an unknown/blank role to the safe default (builder).
func normalizeRole(role string) string {
	if role == roleIntegrator {
		return roleIntegrator
	}
	return roleBuilder
}

// serveWith is the testable core of Serve: the dial step is injected so a stub
// backend (and a controllable re-Dial) drives the handlers without a daemon.
func serveWith(ctx context.Context, in io.Reader, out io.Writer, version, role string, logf func(format string, args ...any), cfg coopclient.Config, dial dialFunc) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	s := &server{
		version: version,
		role:    normalizeRole(role),
		cfg:     cfg,
		dial:    dial,
		logf:    logf,
		claimed: map[string]string{}, // path -> lease key, for auto-renew + cleanup
		out:     out,
	}

	// FAIL-SOFT pre-warm: dialing here primes the connection, but a failure is
	// swallowed — the server runs daemon-less and every tool degrades to its
	// fail-soft note. We never refuse to serve.
	if b, err := dial(cfg); err == nil && b != nil {
		s.be = b
	} else if err != nil {
		logf("mcp: initial dial failed (serving fail-soft, coordination unavailable): %v", err)
	}

	// Lifecycle teardown runs exactly once, on whichever of ctx-cancel / EOF
	// fires first. It best-effort releases every lease this server claimed and
	// closes the connection.
	defer s.shutdown()

	// Integrator singleton (ENFORCED — exactly one integrator per trunk). Before
	// serving a single request we SYNCHRONOUSLY acquire the well-known presence
	// lease. A reachable daemon that denies it means another live integrator
	// already holds it — we REFUSE, returning here without serving any tools.
	// FAIL-SOFT (Inv 13): if the daemon is unreachable / the RPC fails we cannot
	// verify the singleton, so we DEGRADE TO ALLOWING and serve advisory — never
	// more fragile than a bare session. Builders take no presence lease and are
	// never refused.
	if s.role == roleIntegrator {
		if err := s.acquireIntegratorPresence(); err != nil {
			return err
		}
	}

	// Auto-renew: a single goroutine renews every tracked claim so a long tool
	// session never lets its leases lapse. It exits when serveCtx is cancelled.
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// FAIL-SOFT (Inv 13): the renew loop must never take down the server. A
		// panic here (e.g. a future/swapped backend) is recovered and logged — the
		// worst case degrades to leases lapsing at their TTL, never a crashed
		// server, mirroring the tool-dispatch panic guard.
		defer func() {
			if r := recover(); r != nil {
				s.logf("mcp: renew loop panic recovered: %v", r)
			}
		}()
		s.renewLoop(serveCtx)
	}()

	// Integrator presence renew: the synchronous acquire above already gated the
	// refuse decision and holds the lease; this goroutine only RENEWS it for the
	// session lifetime (re-acquiring if it ever lapses) and releases it on exit.
	// Builders never run this.
	if s.role == roleIntegrator {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					s.logf("mcp: presence loop panic recovered: %v", r)
				}
			}()
			s.presenceRenewLoop(serveCtx)
		}()
	}

	if os.Getenv("MAD_LAUNCHED") == "" {
		if branch, ok := s.integrationEventBranch(); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						s.logf("mcp: integration event inbox panic recovered: %v", r)
					}
				}()
				s.integrationEventInboxLoop(serveCtx, branch)
			}()
		}
	}

	defer wg.Wait()
	defer cancel() // stop renew before shutdown releases leases

	err := s.readLoop(serveCtx, in)
	cancel()
	return err
}

// server is one MCP session's mutable state. claimed (guarded by mu) is the
// path->leaseKey set this server currently holds; it is read by status/locks,
// mutated by claim/release, and walked by the renew ticker — hence the mutex,
// even though coopclient.Client is itself concurrency-safe.
type server struct {
	version string
	role    string // "builder" (default) or "integrator"
	cfg     coopclient.Config
	dial    dialFunc
	logf    func(format string, args ...any)

	// Seams for the integrator approve flow, overridable in tests. A nil seam
	// falls back to the production default (os.Getwd / a fresh daemon Dial /
	// conductor.Converge) at call time.
	getwd    func() (string, error)
	dialRPC  func() (rpcConn, error)
	converge func(conductor.RPCClient, conductor.Spec) conductor.Result

	mu      sync.Mutex
	be      backend           // nil when no daemon connection (fail-soft)
	claimed map[string]string // repo-relative path -> daemon-minted lease key

	// presenceKey is the integrator presence/slot lease key this session renews.
	// It is set once by acquireIntegratorPresence (synchronously, before the
	// presence goroutine is started — the go statement provides the happens-before)
	// and only read by presenceRenewLoop. For the default singleton (pool size 1)
	// it is the well-known mad-substrate:integrator:v1 key; for an opt-in pool it is
	// the specific slot key this integrator acquired.
	presenceKey string

	// presencePidfile is the path of the pidfile this server wrote when it
	// acquired presence (empty if none). `integrator stop` reads it to find and
	// signal this process; the renew loop / shutdown remove it on release. Guarded
	// by mu (written by acquireIntegratorPresence, cleared on removal).
	presencePidfile string

	eventMu    sync.Mutex
	eventInbox []string
	eventLast  string

	out io.Writer
}

// rpcConn is the fresh-connection surface the integrator approve flow dials: a
// daemon RPC channel (satisfying conductor.RPCClient) that must be Closed when
// the convergence finishes. *rpcclient.Client satisfies it.
type rpcConn interface {
	Call(method string, params any, result any) error
	Close() error
}

// readLoop consumes newline-delimited JSON-RPC from in, dispatching each
// request and writing each response to out. It returns nil on EOF or ctx-cancel
// (both are normal shutdowns), and a non-nil error only on an unrecoverable
// write/read failure.
func (s *server) readLoop(ctx context.Context, in io.Reader) error {
	br := bufio.NewReaderSize(in, 64<<10)

	// Read in a separate goroutine so a ctx-cancel (SIGINT/SIGTERM/SIGHUP) unblocks
	// us even while the stdin read is PARKED waiting for the next line. Checking
	// ctx.Done() only between reads (the old shape) left the process hung on a
	// blocking read until the agent host closed the stream — so a signalled
	// integrator released its presence lease (separate goroutine) but never exited.
	// The reader goroutine may stay blocked on that final read; that is fine, the
	// process is tearing down. Dispatch stays single-threaded: only this loop calls
	// handleLine.
	type lineRead struct {
		line []byte
		err  error
	}
	reads := make(chan lineRead, 1)
	go func() {
		for {
			line, err := readLine(br, maxLine)
			reads <- lineRead{line, err}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-reads:
			if len(r.line) > 0 {
				if wErr := s.handleLine(r.line); wErr != nil {
					return wErr
				}
			}
			if r.err != nil {
				if r.err == io.EOF {
					return nil
				}
				// A too-long line or a read error ends the session; the agent host
				// owns the stream lifecycle. Log and stop rather than spin.
				s.logf("mcp: read: %v", r.err)
				return nil
			}
		}
	}
}

// readLine reads one '\n'-terminated line (without the newline), enforcing the
// max length. It returns the line bytes (possibly empty) and io.EOF when the
// stream ends. An over-length line returns errLineTooLong.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	line = bytes.TrimRight(line, "\r\n")
	if len(line) > max {
		return nil, errLineTooLong
	}
	return line, err
}

var errLineTooLong = &transportErr{"mcp: message exceeds max line length"}

type transportErr struct{ msg string }

func (e *transportErr) Error() string { return e.msg }

// handleLine parses one line and dispatches it. A blank line is skipped. An
// unparseable line yields a -32700 error ONLY if an id can be recovered (a
// response with no matching id is useless), else it is dropped. Notifications
// (no id) produce no output. Requests always produce exactly one response line.
func (s *server) handleLine(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}

	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		// Try to salvage an id so the client can correlate the parse error.
		if id, ok := salvageID(line); ok {
			return s.writeMessage(errorResponse(id, codeParseError, "parse error"))
		}
		s.logf("mcp: drop unparseable line (no id): %v", err)
		return nil
	}

	// A request HAS an id (even JSON null counts as present per JSON-RPC); a
	// notification has none. We must respond to requests and stay silent on
	// notifications.
	isNotification := req.ID == nil

	resp, respond := s.dispatch(&req)
	if isNotification || !respond {
		return nil
	}
	return s.writeMessage(resp)
}

// dispatch routes a parsed request to its handler. respond is false for methods
// that produce no reply (notifications/initialized). The returned response
// already carries the request id.
func (s *server) dispatch(req *rpcRequest) (resp rpcResponse, respond bool) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req), true
	case "notifications/initialized":
		return rpcResponse{}, false
	case "ping":
		return resultResponse(req.ID, struct{}{}), true
	case "tools/list":
		return resultResponse(req.ID, s.toolsList()), true
	case "tools/call":
		return s.handleToolsCall(req), true
	default:
		return errorResponse(req.ID, codeMethodNotFound, "method not found"), true
	}
}

// writeMessage marshals a response to compact JSON and writes it as one line.
func (s *server) writeMessage(resp rpcResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		// A response we built ourselves should always marshal; if it somehow
		// cannot, there is nothing useful to send. Log and continue.
		s.logf("mcp: marshal response: %v", err)
		return nil
	}
	b = append(b, '\n')
	_, wErr := s.out.Write(b)
	return wErr
}

// renewLoop renews every tracked claim on a ticker. On a renew that returns
// not-ok or errors, the key is dropped from the tracked set so status never
// claims a lease this server no longer holds (Inv 4 honesty). A transport error
// triggers ONE re-Dial attempt.
func (s *server) renewLoop(ctx context.Context) {
	interval := s.cfg.LeaseTTL / 2
	if interval < minRenewInterval {
		interval = minRenewInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.renewAll()
		}
	}
}

// renewAll renews each tracked claim, dropping any key that no longer renews.
func (s *server) renewAll() {
	// Snapshot under the lock so we don't hold mu across RPCs.
	s.mu.Lock()
	be := s.be
	keys := make(map[string]string, len(s.claimed))
	for path, key := range s.claimed {
		keys[path] = key
	}
	s.mu.Unlock()

	if be == nil || len(keys) == 0 {
		return
	}

	for path, key := range keys {
		ok, err := be.Renew(key, s.cfg.LeaseTTL)
		if err != nil && coopclient.IsTransport(err) {
			// A transport blip during renew is fail-soft: try one re-Dial and
			// retry this key once. A still-failing daemon just means the lease
			// will lapse — safe, since the worktree is isolated.
			if nb := s.redial(); nb != nil {
				be = nb
				ok, err = be.Renew(key, s.cfg.LeaseTTL)
			}
		}
		if err != nil || !ok {
			// Stop lying about a lease we could not keep.
			s.untrack(path)
			s.logf("mcp: dropped lapsed claim %q (ok=%v err=%v)", path, ok, err)
		}
	}
}

// shutdown is the best-effort teardown: release every claimed lease and close
// the connection. It is idempotent and never panics.
func (s *server) shutdown() {
	// Belt-and-suspenders: the presence renew loop removes the pidfile on ctx-
	// cancel, but shutdown also runs on the fail-soft/refused paths — clear it here
	// too so a stale pidfile never outlives this process.
	s.removePresencePidfile()
	s.mu.Lock()
	be := s.be
	claims := make([]string, 0, len(s.claimed))
	for _, key := range s.claimed {
		claims = append(claims, key)
	}
	s.claimed = map[string]string{}
	s.be = nil
	s.mu.Unlock()

	if be != nil {
		for _, key := range claims {
			// Best-effort: a failed release just lets the lease expire on TTL.
			if _, err := be.Release(key); err != nil {
				s.logf("mcp: release on shutdown: %v", err)
			}
		}
	}
	if be != nil {
		if err := be.Close(); err != nil {
			s.logf("mcp: close: %v", err)
		}
	}
}

// track records a granted claim (path -> lease key) for auto-renew + cleanup.
func (s *server) track(path, key string) {
	s.mu.Lock()
	s.claimed[path] = key
	s.mu.Unlock()
}

// untrack stops tracking a claim (on release or a failed renew).
func (s *server) untrack(path string) {
	s.mu.Lock()
	delete(s.claimed, path)
	s.mu.Unlock()
}

// integratorPresenceKey is the base64 of the well-known presence key bytes
// `mad-substrate:integrator:v1`. A sibling CLI reads it via lease.inspect to learn
// whether an integrator MCP server is already live.
var integratorPresenceKey = base64.StdEncoding.EncodeToString([]byte("mad-substrate:integrator:v1"))

// integratorPresenceTTL is the presence lease TTL; renew runs at half this.
const integratorPresenceTTL = 60 * time.Second

// integratorPoolEnv is the OPT-IN knob for the integrator POOL size. Empty /
// unparseable / <=1 ⇒ N=1 ⇒ EXACTLY the singleton behavior (one integrator per
// trunk, the SAME mad-substrate:integrator:v1 key the CLI status inspects).
const integratorPoolEnv = "MAD_INTEGRATOR_POOL"

// integratorPoolSize reads integratorPoolEnv. Default/empty/unparseable/<=1 ⇒ 1.
func integratorPoolSize() int {
	v := strings.TrimSpace(os.Getenv(integratorPoolEnv))
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// integratorSlotKeys returns the base64 presence-lease keys for an N-slot pool,
// in slot order. For N<=1 it returns EXACTLY the singleton key
// mad-substrate:integrator:v1 (byte-identical to the pre-pool behavior — the same key
// `mad-substrate integrator status` inspects). For N>1 it returns one key per slot:
// mad-substrate:integrator:v1:slot-0 ... slot-(N-1).
//
// Rationale (R9): running N>1 integrators concurrently is SAFE — it is
// convergence-plane PARALLELISM, not a mesh. (a) Each integrator claims DISTINCT
// pending branches via the per-record integration.claim CAS, so no two ever work
// the same request; (b) every actual merge still serializes on the single trunk
// lease inside conductor.Converge. The slots only bound how many integrators may
// run at once; they add NO cross-integrator messaging.
func integratorSlotKeys(n int) []string {
	raw := integratorSlotRawKeys(n)
	keys := make([]string, len(raw))
	for i, key := range raw {
		keys[i] = base64.StdEncoding.EncodeToString(key)
	}
	return keys
}

func integratorSlotRawKeys(n int) [][]byte {
	if n <= 1 {
		return [][]byte{[]byte("mad-substrate:integrator:v1")}
	}
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		raw := fmt.Sprintf("mad-substrate:integrator:v1:slot-%d", i)
		keys[i] = []byte(raw)
	}
	return keys
}

// acquireIntegratorPresence performs the ONE synchronous, refuse-gating acquire
// of an integrator presence/slot lease, run before the server serves any request.
// With the default pool size 1 there is a single slot (the well-known
// mad-substrate:integrator:v1 key) and the behavior is byte-identical to the historic
// singleton. With an opt-in pool (N>1) it acquires the FIRST FREE slot among
// slot-0..slot-(N-1).
//
// It returns a non-nil error ONLY when EVERY slot is held by another live
// integrator (the pool is full) — in which case the caller refuses to serve.
// Every other outcome returns nil (proceed):
//
//   - daemon unreachable / acquire RPC failed → FAIL-SOFT (Inv 13): we cannot
//     verify the pool, so we degrade to allowing and serve advisory rather than
//     make a governed session more fragile than a bare one.
//   - a free slot granted → we now hold that slot; presenceRenewLoop keeps it.
func (s *server) acquireIntegratorPresence() error {
	keys := integratorSlotKeys(integratorPoolSize())
	// Default the renew target to the first slot so a fail-soft proceed (no slot
	// acquired) still has presenceRenewLoop attempt (re)acquisition. For N==1 this
	// is the singleton key, byte-identical to the pre-pool behavior.
	s.presenceKey = keys[0]

	be := s.backendOrRedial()
	if be == nil {
		// No daemon reachable: cannot verify the pool — fail-soft, proceed.
		s.logf("mcp: integrator presence could not be verified (daemon unreachable); proceeding advisory (Inv 13)")
		return nil
	}
	var lastHolder string
	for _, key := range keys {
		granted, holder, _, err := be.Acquire(key, integratorPresenceTTL)
		if err != nil {
			// Acquire RPC failed: fail-soft, proceed advisory (Inv 13).
			s.logf("mcp: integrator presence could not be verified (presence acquire failed: %v); proceeding advisory (Inv 13)", err)
			return nil
		}
		if granted {
			// We hold this slot; renew it for the session lifetime.
			s.presenceKey = key
			// Record our pid beside the ledger so `integrator stop` can find and
			// signal this exact process (best-effort; a write failure just means
			// stop falls back to the TTL reclaim).
			s.writePresencePidfile(key)
			return nil
		}
		lastHolder = holder
	}
	// Every slot is held by another live integrator: the enforced refusal.
	if len(keys) == 1 {
		return fmt.Errorf("mcp: an integrator is already running (holder %s); only one integrator per trunk", lastHolder)
	}
	return fmt.Errorf("mcp: all %d integrator slots are in use (most recent holder %s); raise %s or wait for a slot to free", len(keys), lastHolder, integratorPoolEnv)
}

// presenceRenewLoop renews the already-held integrator presence lease for the
// session lifetime. The refuse-gating acquire happened synchronously in
// acquireIntegratorPresence; this loop never re-runs that decision. FAIL-SOFT
// (Inv 13): a transport failure is swallowed and, if the lease ever lapses, it
// is best-effort re-acquired so a dead integrator's presence is reclaimed once
// its lease expires. On ctx-cancel it best-effort releases the presence lease.
func (s *server) presenceRenewLoop(ctx context.Context) {
	// presenceKey is the slot this session acquired (or, on a fail-soft proceed,
	// slot-0 / the singleton key as the re-acquire target). It was set
	// synchronously by acquireIntegratorPresence before this goroutine started.
	key := s.presenceKey
	if key == "" {
		key = integratorPresenceKey
	}
	t := time.NewTicker(integratorPresenceTTL / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if be := s.backendOrRedial(); be != nil {
				_, _ = be.Release(key)
			}
			// Our presence is gone: drop the pidfile so a subsequent `integrator
			// stop` never signals a pid we no longer own.
			s.removePresencePidfile()
			return
		case <-t.C:
			be := s.backendOrRedial()
			if be == nil {
				continue
			}
			// Renew; if we never held it (or it lapsed), try to (re)acquire so we
			// take over presence once a dead integrator's lease expires.
			if ok, err := be.Renew(key, integratorPresenceTTL); err != nil || !ok {
				_, _, _, _ = be.Acquire(key, integratorPresenceTTL)
			}
		}
	}
}

func (s *server) integrationEventBranch() (string, bool) {
	if s.role == roleIntegrator {
		return "", true
	}
	branch, err := s.ownBranch()
	if err != nil {
		s.logf("mcp: integration event inbox disabled; cannot resolve builder branch: %v", err)
		return "", false
	}
	return branch, true
}

func (s *server) integrationEventInboxLoop(ctx context.Context, branch string) {
	poll := func() {
		be := s.backendOrRedial()
		if be == nil {
			return
		}
		events, err := be.IntegrationEvents(branch, integrationEventPollMax)
		if err != nil {
			// Fail-soft (Inv 13): event delivery is advisory; polling errors mean
			// no nudges, never a broken tool result.
			s.logf("mcp: integration event poll failed: %v", err)
			return
		}
		s.enqueueIntegrationEvents(events)
	}

	poll()
	t := time.NewTicker(integrationEventPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			poll()
		}
	}
}

func (s *server) enqueueIntegrationEvents(events []coopclient.IntegrationEvent) {
	requested := 0
	for _, ev := range events {
		// A requeued request (dead claimer) is work awaiting review again — fold
		// it into the same "awaiting review" count as a fresh request.
		if ev.Kind == "integration.requested" || ev.Kind == "integration.requeued" {
			requested++
			continue
		}
		if line, ok := renderIntegrationNudge(ev.Kind, ev.Branch, 1); ok {
			s.enqueueIntegrationNudge(line)
		}
	}
	if requested > 0 {
		if line, ok := renderIntegrationNudge("integration.requested", "", requested); ok {
			s.enqueueIntegrationNudge(line)
		}
	}
}

func (s *server) enqueueIntegrationNudge(line string) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if line == "" || line == s.eventLast {
		return
	}
	s.eventInbox = append(s.eventInbox, line)
	s.eventLast = line
}

func (s *server) drainIntegrationNudges() []string {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if len(s.eventInbox) == 0 {
		return nil
	}
	lines := append([]string(nil), s.eventInbox...)
	s.eventInbox = nil
	return lines
}

// writePresencePidfile records THIS integrator MCP server's OS pid in a pidfile
// beside the ledger so `mad-substrate integrator stop` can find and SIGTERM it (the
// signal the server handles by releasing its presence lease — the tested clean-
// release path). keyB64 is the base64 presence slot key this server holds. Best-
// effort: any failure is logged and ignored, leaving stop to fall back to the TTL
// reclaim. Removed by removePresencePidfile on release/shutdown.
func (s *server) writePresencePidfile(keyB64 string) {
	raw, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return
	}
	path := runtimecfg.IntegratorPidfile(runtimecfg.SocketPath(""), raw)
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		s.logf("mcp: could not write integrator pidfile %s: %v", path, err)
		return
	}
	s.mu.Lock()
	s.presencePidfile = path
	s.mu.Unlock()
}

// removePresencePidfile deletes the pidfile written by writePresencePidfile, if
// any. Idempotent and best-effort — a missing file is not an error.
func (s *server) removePresencePidfile() {
	s.mu.Lock()
	path := s.presencePidfile
	s.presencePidfile = ""
	s.mu.Unlock()
	if path != "" {
		_ = os.Remove(path)
	}
}

// defaultDialRPC dials a fresh daemon connection for the integrator approve
// flow. The connection is the integrator's own (separate from the persistent
// MCP backend) so conductor.Converge serializes the merge through its own lease.
func defaultDialRPC() (rpcConn, error) {
	c, err := rpcclient.Dial(runtimecfg.SocketPath(""))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// backendOrRedial returns the live backend, attempting ONE re-Dial if there is
// none (or after a transport error). It returns nil when no daemon is reachable
// — callers then emit the fail-soft note.
func (s *server) backendOrRedial() backend {
	s.mu.Lock()
	be := s.be
	s.mu.Unlock()
	if be != nil {
		return be
	}
	return s.redial()
}

// redial attempts a single reconnection, swapping in the new backend on success.
// A failure leaves the server daemon-less (fail-soft) and returns nil.
func (s *server) redial() backend {
	be, err := s.dial(s.cfg)
	if err != nil || be == nil {
		if err != nil {
			s.logf("mcp: re-dial failed (fail-soft): %v", err)
		}
		return nil
	}
	s.mu.Lock()
	s.be = be
	s.mu.Unlock()
	return be
}
