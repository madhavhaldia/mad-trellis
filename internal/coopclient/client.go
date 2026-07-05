// Package coopclient is the shared "cooperative client" half of mad-trellis's
// native-Go cooperative layer — the in-process Go counterpart of the (now
// deleted) TypeScript adapter. It is a SECOND client of the daemon's FROZEN
// JSON-RPC registry (the first being the CLI's out-of-process rpcclient
// callers): it adds NO daemon methods and codes only against the frozen wire
// surface {session,classify,lease,integrate}.
//
// Two invariants shape this package and are commented at their load-bearing
// sites:
//
//   - Inv 4 (connection-bound identity): the lease holder is the daemon-minted
//     identity of the CONNECTION, never a parameter. One Client == one
//     persistent connection == one stable identity. We therefore hold a single
//     rpcclient connection for the Client's life and serialize every Call behind
//     a mutex; we never pass a holder/session as an RPC argument.
//
//   - Inv 13 (fail-soft is the law): mad-trellis advises, it does not block work.
//     A daemon that is slow, unreachable, or returns a malformed reply must
//     never deny an edit. Every method resolves ambiguity toward best-effort and
//     never throws.
//
// SAFE FOR CONCURRENT USE: the MCP server renews leases on a ticker while
// serving tool calls, so Client methods are guarded by a mutex. The underlying
// rpcclient is single-connection and not itself concurrency-safe, so the mutex
// is what makes concurrent use correct.
package coopclient

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/rpcclient"
	"github.com/madhavhaldia/mad-trellis/internal/runtimecfg"
)

// Default lease/RPC parameters used when the corresponding env var is absent,
// empty, or non-positive.
const (
	defaultLeaseTTL   = 60 * time.Second
	defaultRPCTimeout = 2 * time.Second
)

// Config is the resolved cooperative-client configuration: socket + identity
// correlation + lease/RPC timing. It is produced by Load from the environment
// and the shared runtimecfg resolver so a governed agent and the CLI always
// agree on the same socket.
type Config struct {
	Socket     string        // resolved daemon socket path
	Session    string        // MAD_SESSION (correlation id; "" if unset) — NOT the lease holder
	Token      string        // MAD_SESSION_TOKEN ("" if unset)
	LeaseTTL   time.Duration // claim TTL
	RPCTimeout time.Duration // per-call ceiling
}

// Load reads the cooperative-client environment, applies the documented
// defaults, and resolves the socket via runtimecfg so this client lands on the
// EXACT socket the CLI and daemon use.
//
// MAD_SESSION is a correlation id only — it is NOT the lease holder. The
// holder is minted by the daemon per connection (Inv 4) and read back via
// session.whoami after Dial.
func Load() Config {
	return Config{
		// runtimecfg owns the socket precedence: MAD_SOCKET wins, else
		// <runtime-dir>/daemon.sock where runtime-dir is
		// MAD_RUNTIME_DIR > MAD_HOME > ~/.mad-trellis.
		Socket:     runtimecfg.SocketPath(""),
		Session:    strings.TrimSpace(os.Getenv("MAD_SESSION")),
		Token:      strings.TrimSpace(os.Getenv("MAD_SESSION_TOKEN")),
		LeaseTTL:   durFromEnvMs(os.Getenv("MAD_LEASE_TTL_MS"), defaultLeaseTTL),
		RPCTimeout: durFromEnvMs(os.Getenv("MAD_RPC_TIMEOUT_MS"), defaultRPCTimeout),
	}
}

// durFromEnvMs parses an integer-millisecond env value, returning def when the
// value is absent, unparseable, or non-positive. A bad knob never zeroes the
// TTL/timeout (which would make every claim immediately expire / every call
// instantly time out) — it falls back to the safe default.
func durFromEnvMs(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Millisecond
}

// Holder is one entry of lease.list — the daemon-minted holder of a lease key.
type Holder struct {
	Key         string
	Holder      string
	ExpiresAtMs int64
	Fence       int64
}

// Integration is one entry of integrate.list.
type Integration struct {
	ID          string
	Branch      string
	Holder      string
	State       string
	Base        string
	MergeCommit string
}

// PendingIntegration is one entry of integration.pending — a builder's request
// awaiting an integrator's verdict.
type PendingIntegration struct {
	ID          string
	Branch      string
	Title       string
	State       string
	CreatedAtMs int64
}

// IntegrationEvent is one daemon-authored wake-up from integration.events.
// It deliberately carries no agent-authored payload; request rows remain the
// durable truth, while events are just nudges to re-read that truth.
type IntegrationEvent struct {
	ID          int64
	Kind        string
	Branch      string
	CreatedAtMs int64
}

// LeaseView is the read-only result of lease.inspect: the daemon's view of a
// single key (whether a row exists, who holds it, and whether the lease is
// currently live).
type LeaseView struct {
	Exists      bool
	Holder      string
	ExpiresAtMs int64
	Fence       int64
	Held        bool
}

// Error carries the daemon/transport code so callers can distinguish a daemon
// VERDICT (a structured JSON-RPC error code, e.g. CodeAuthz/-32001 or
// CodeConflict/-32003) from an UNREACHABLE/slow daemon. Code 0 means
// transport/timeout (IsTransport): no daemon verdict was reached.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// IsTransport reports whether this Error is a transport/timeout failure (no
// daemon verdict). Callers that must fail soft on an unreachable daemon key off
// this rather than the message text.
func (e *Error) IsTransport() bool { return e.Code == 0 }

// IsTransport is the convenience form: true when err is a transport/timeout
// *Error (Code 0). A nil error is NOT a transport error.
func IsTransport(err error) bool {
	var e *Error
	if asErr(err, &e) {
		return e.IsTransport()
	}
	return false
}

// Client is connection-bound: one Client owns one persistent rpcclient
// connection and therefore one stable daemon-minted identity (Inv 4). Every
// method serializes behind mu so the Client is safe for concurrent use (the MCP
// server renews on a ticker while serving tool calls), and so the single
// underlying connection is never touched by two goroutines at once.
type Client struct {
	mu     sync.Mutex
	rc     *rpcclient.Client
	holder string // daemon-minted holder for THIS connection (session.whoami)
}

// Dial opens a persistent connection to the daemon. THEN, if a token is set, it
// calls session.attach to bind this connection to the launcher's session —
// FAIL-SOFT: on ANY attach error (dead/bad token, CodeAuthz, transport) it
// keeps the per-connection minted identity and never fails the Dial. THEN it
// calls session.whoami to record the holder for Holder().
//
// Attach is fail-soft because a missing/expired token must degrade a governed
// agent to its own ungoverned-but-still-coordinating identity, not crash it:
// the connection ALWAYS has a daemon-minted identity even without attach.
func Dial(cfg Config) (*Client, error) {
	rc, err := rpcclient.Dial(cfg.Socket, rpcclient.WithReadTimeout(cfg.RPCTimeout))
	if err != nil {
		return nil, &Error{Code: 0, Message: fmt.Sprintf("dial %s: %v", cfg.Socket, err)}
	}
	c := &Client{rc: rc}

	if cfg.Token != "" {
		var att struct {
			Session string `json:"session"`
		}
		// Fail-soft: ignore the error AND the result. A failed attach leaves
		// the connection on its own minted identity, which is correct — we do
		// not adopt a holder from a half-failed attach.
		_ = rc.Call("session.attach", params(map[string]any{"token": cfg.Token}), &att)
	}

	var who struct {
		Session string `json:"session"`
	}
	if err := rc.Call("session.whoami", params(nil), &who); err == nil {
		c.holder = who.Session
	}
	return c, nil
}

// Close closes the underlying connection. Releasing leases is the caller's
// responsibility before Close (mirrors rpcclient).
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rc.Close()
}

// Holder returns this connection's daemon-minted holder (from session.whoami at
// Dial). Empty if whoami failed — callers treat an empty holder as "cannot
// attribute", never as a real holder.
func (c *Client) Holder() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holder
}

// Route resolves a resource to its coordination kind and lease key.
// leaseKey is "" when the daemon returns a null lease_key (a forkable resource
// that needs no coordination). The lease key is an opaque base64 string minted
// by the daemon (Inv 9) — callers must never synthesize it.
func (c *Client) Route(domain, name string) (kind, leaseKey string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Kind     string  `json:"kind"`
		LeaseKey *string `json:"lease_key"`
	}
	if err := c.call("classify.route", map[string]any{"domain": domain, "name": name}, &out); err != nil {
		return "", "", err
	}
	if out.LeaseKey != nil {
		leaseKey = *out.LeaseKey
	}
	return out.Kind, leaseKey, nil
}

// Acquire claims the lease key for ttl. The holder is connection-bound and is
// NEVER sent as a parameter (Inv 4) — the returned holder is the daemon's view
// of who holds the key after the attempt.
func (c *Client) Acquire(leaseKey string, ttl time.Duration) (granted bool, holder string, fence int64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Granted     bool   `json:"granted"`
		Holder      string `json:"holder"`
		ExpiresAtMs int64  `json:"expires_at_ms"`
		Fence       int64  `json:"fence"`
	}
	if err := c.call("lease.acquire", map[string]any{"key": leaseKey, "ttl_ms": ttlMs(ttl)}, &out); err != nil {
		return false, "", 0, err
	}
	return out.Granted, out.Holder, out.Fence, nil
}

// Renew extends the lease key by ttl. ok is the daemon's verdict (false when
// this connection no longer holds the key).
func (c *Client) Renew(leaseKey string, ttl time.Duration) (ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		OK          bool  `json:"ok"`
		ExpiresAtMs int64 `json:"expires_at_ms"`
	}
	if err := c.call("lease.renew", map[string]any{"key": leaseKey, "ttl_ms": ttlMs(ttl)}, &out); err != nil {
		return false, err
	}
	return out.OK, nil
}

// Release drops the lease key held by this connection.
func (c *Client) Release(leaseKey string) (ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.call("lease.release", map[string]any{"key": leaseKey}, &out); err != nil {
		return false, err
	}
	return out.OK, nil
}

// ListHolders returns every currently held lease key and its holder.
func (c *Client) ListHolders() ([]Holder, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Holders []struct {
			Key         string `json:"key"`
			Holder      string `json:"holder"`
			ExpiresAtMs int64  `json:"expires_at_ms"`
			Fence       int64  `json:"fence"`
		} `json:"holders"`
	}
	if err := c.call("lease.list", nil, &out); err != nil {
		return nil, err
	}
	holders := make([]Holder, 0, len(out.Holders))
	for _, h := range out.Holders {
		holders = append(holders, Holder{Key: h.Key, Holder: h.Holder, ExpiresAtMs: h.ExpiresAtMs, Fence: h.Fence})
	}
	return holders, nil
}

// Submit registers a branch for integration and returns its assigned id,
// initial state, and base.
func (c *Client) Submit(branch string) (id, state, base string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Base  string `json:"base"`
	}
	if err := c.call("integrate.submit", map[string]any{"branch": branch}, &out); err != nil {
		return "", "", "", err
	}
	return out.ID, out.State, out.Base, nil
}

// Inspect reads the daemon's read-only view of a single lease key
// (lease.inspect). leaseKey is the opaque base64 key minted by the daemon
// (Inv 9); a sibling caller uses this to discover whether an integrator's
// presence lease is currently live.
func (c *Client) Inspect(leaseKey string) (LeaseView, error) {
	return c.inspectEncodedLeaseKey(leaseKey)
}

// LeaseInspect reads lease.inspect for a raw lease key, encoding it exactly as
// the daemon wire contract expects. This is the MCP-facing shape for presence
// checks that construct well-known raw keys locally.
func (c *Client) LeaseInspect(key []byte) (LeaseView, error) {
	return c.inspectEncodedLeaseKey(base64.StdEncoding.EncodeToString(key))
}

func (c *Client) inspectEncodedLeaseKey(leaseKey string) (LeaseView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Exists      bool   `json:"exists"`
		Holder      string `json:"holder"`
		ExpiresAtMs int64  `json:"expires_at_ms"`
		Fence       int64  `json:"fence"`
		Held        bool   `json:"held"`
	}
	if err := c.call("lease.inspect", map[string]any{"key": leaseKey}, &out); err != nil {
		return LeaseView{}, err
	}
	return LeaseView{Exists: out.Exists, Holder: out.Holder, ExpiresAtMs: out.ExpiresAtMs, Fence: out.Fence, Held: out.Held}, nil
}

// RequestIntegration (builder side) registers this branch for integration and
// returns the assigned id and initial state. title is optional; an empty title
// is omitted from the request.
func (c *Client) RequestIntegration(branch, title string) (id, state string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	p := map[string]any{"branch": branch}
	if title != "" {
		p["title"] = title
	}
	if err := c.call("integration.request", p, &out); err != nil {
		return "", "", err
	}
	return out.ID, out.State, nil
}

// IntegrationStatus (builder side) reports the current state of this branch's
// integration request, including any reviewer feedback. found is false when no
// request exists for the branch.
func (c *Client) IntegrationStatus(branch string) (found bool, id, state, feedback, merge string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Found    bool   `json:"found"`
		ID       string `json:"id"`
		State    string `json:"state"`
		Feedback string `json:"feedback"`
		Merge    string `json:"merge"`
	}
	if err := c.call("integration.status", map[string]any{"branch": branch}, &out); err != nil {
		return false, "", "", "", "", err
	}
	return out.Found, out.ID, out.State, out.Feedback, out.Merge, nil
}

// IntegrationPending (integrator side) lists every request awaiting a verdict.
func (c *Client) IntegrationPending() ([]PendingIntegration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Pending []struct {
			ID          string `json:"id"`
			Branch      string `json:"branch"`
			Title       string `json:"title"`
			State       string `json:"state"`
			CreatedAtMs int64  `json:"created_at_ms"`
		} `json:"pending"`
	}
	if err := c.call("integration.pending", nil, &out); err != nil {
		return nil, err
	}
	ps := make([]PendingIntegration, 0, len(out.Pending))
	for _, p := range out.Pending {
		ps = append(ps, PendingIntegration{ID: p.ID, Branch: p.Branch, Title: p.Title, State: p.State, CreatedAtMs: p.CreatedAtMs})
	}
	return ps, nil
}

// IntegrationClaim (integrator side) claims a pending request for review. ok is
// false when it is no longer pending (already claimed/decided).
func (c *Client) IntegrationClaim(id string) (ok bool, branch, title string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		OK     bool   `json:"ok"`
		Branch string `json:"branch"`
		Title  string `json:"title"`
	}
	if err := c.call("integration.claim", map[string]any{"id": id}, &out); err != nil {
		return false, "", "", err
	}
	return out.OK, out.Branch, out.Title, nil
}

// IntegrationVerdict (integrator side) records the decision for a claimed
// request. decision is "approve" or "reject"; feedback and merge are omitted
// when empty (merge is the merge-commit OID recorded on an approve).
func (c *Client) IntegrationVerdict(id, decision, feedback, merge string) (ok bool, state string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		OK    bool   `json:"ok"`
		State string `json:"state"`
	}
	p := map[string]any{"id": id, "decision": decision}
	if feedback != "" {
		p["feedback"] = feedback
	}
	if merge != "" {
		p["merge"] = merge
	}
	if err := c.call("integration.verdict", p, &out); err != nil {
		return false, "", err
	}
	return out.OK, out.State, nil
}

// IntegrationEvents polls daemon-authored wake-up events for branch. An empty
// branch asks for integrator-audience events readable by this connection.
func (c *Client) IntegrationEvents(branch string, max int) ([]IntegrationEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Events []struct {
			ID          int64  `json:"id"`
			Kind        string `json:"kind"`
			Branch      string `json:"branch"`
			CreatedAtMs int64  `json:"created_at_ms"`
		} `json:"events"`
	}
	p := map[string]any{}
	if branch != "" {
		p["branch"] = branch
	}
	if max > 0 {
		p["max"] = max
	}
	if err := c.call("integration.events", p, &out); err != nil {
		return nil, err
	}
	events := make([]IntegrationEvent, 0, len(out.Events))
	for _, ev := range out.Events {
		events = append(events, IntegrationEvent{
			ID: ev.ID, Kind: ev.Kind, Branch: ev.Branch, CreatedAtMs: ev.CreatedAtMs,
		})
	}
	return events, nil
}

// Integrations lists every registered integration.
func (c *Client) Integrations() ([]Integration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out struct {
		Integrations []struct {
			ID          string `json:"id"`
			Branch      string `json:"branch"`
			Holder      string `json:"holder"`
			State       string `json:"state"`
			Base        string `json:"base"`
			MergeCommit string `json:"merge_commit"`
		} `json:"integrations"`
	}
	if err := c.call("integrate.list", nil, &out); err != nil {
		return nil, err
	}
	ints := make([]Integration, 0, len(out.Integrations))
	for _, i := range out.Integrations {
		ints = append(ints, Integration{
			ID: i.ID, Branch: i.Branch, Holder: i.Holder,
			State: i.State, Base: i.Base, MergeCommit: i.MergeCommit,
		})
	}
	return ints, nil
}

// call wraps rpcclient.Call, always sending a params object (no-arg calls send
// {}), and converts a flattened protocol error back into a typed *Error so
// callers can distinguish a daemon verdict (Code != 0) from an unreachable
// daemon (Code 0). mu is held by every exported method that calls this.
func (c *Client) call(method string, p any, out any) error {
	if err := c.rc.Call(method, params(p), out); err != nil {
		return classifyErr(err)
	}
	return nil
}

// ttlMs converts a duration to whole milliseconds for the wire ttl_ms field.
func ttlMs(d time.Duration) int64 { return int64(d / time.Millisecond) }
