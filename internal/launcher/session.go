// Package launcher implements project 5 (session-launcher-shim): the
// parent-launches-child core that makes mad-trellis governance AMBIENT (Inv 13) —
// launching a supported agent CLI in a governed repo transparently runs it
// inside the isolation boundary the substrate built, with NO change to how the
// user drives the session.
//
// Owns (docs/0003 clause map): Inv 4 + shim-install fail-closed [GATED],
// 13-interaction [AUTO] + no-goals/no-dispatch [GATED], 12-intake [AUTO], and
// clean-exit teardown [GATED]. It DOES NOT build the boundaries it launches into
// (substrate, project 4), speak the cooperative MCP dialect (adapter, 9b), or
// own the crash path (liveness, project 8 — the launcher owns ONLY the normal
// exit).
//
// The cardinal rule (docs/0004 card 5): FAIL-OPEN is the sin. Every error or
// uncertainty on the path to governance — daemon unreachable, identity
// unobtainable, provision refused, shim tampered — defaults to BLOCK. There is
// no code path from a governance failure to running the agent ungoverned.
package launcher

import (
	"fmt"
	"strings"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/substrate"
)

// Conn is the minimal held-connection daemon surface the launcher needs. It is
// an interface (not *rpcclient.Client directly) so the fail-closed paths and the
// clean-exit teardown are unit-testable without a real socket — the adversarial
// tests inject a Conn that simulates an unreachable daemon, a refused provision,
// or a populated lease ledger.
type Conn interface {
	Call(method string, params any, out any) error
	Close() error
}

// Dialer opens a HELD daemon connection. Injected so a test can simulate the
// cardinal fail-closed case (the daemon is down → the dial fails → the launcher
// must BLOCK, never fall through to the bare agent).
type Dialer func(socket string) (Conn, error)

// Session is a HELD daemon connection that IS the launcher's governed identity
// for an agent's entire lifetime.
//
// This single-held-connection discipline is load-bearing and closes chafe C6:
// substrate.provision/teardown are CONNECTION-BOUND (keyed off the daemon's
// unspoofable cc.Session, Inv 4), so the SAME connection that provisions the
// boundary must tear it down. The MSS `spawn` bootstrap was fire-and-forget — it
// provisioned a durable boundary then dropped its connection, leaking the
// daemon's in-memory port reservation + live-map entry on a dead session. The
// launcher holds the connection from provision through clean-exit teardown, so
// the boundary's lifetime equals the session's lifetime.
type Session struct {
	cli Conn
	id  string // the daemon-minted, connection-bound identity (session.whoami)
}

// Open dials a held connection and resolves its daemon-minted identity. ANY
// failure returns an error and NO Session — the caller (Run) translates that
// into a BLOCK. Identity is established up front so the launch is governed from
// the first instant; an unobtainable identity is a fail-closed condition, never
// a "proceed ungoverned" one.
func Open(dial Dialer, socket string) (*Session, error) {
	if dial == nil {
		return nil, fmt.Errorf("launcher: no dialer")
	}
	cli, err := dial(socket)
	if err != nil {
		return nil, fmt.Errorf("daemon unreachable: %w", err)
	}
	var who struct {
		Session string `json:"session"`
	}
	if err := cli.Call("session.whoami", nil, &who); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("identity unobtainable: %w", err)
	}
	if strings.TrimSpace(who.Session) == "" {
		_ = cli.Close()
		return nil, fmt.Errorf("identity unobtainable: daemon returned an empty session")
	}
	return &Session{cli: cli, id: who.Session}, nil
}

// ID is the daemon-minted, connection-bound identity of this session.
func (s *Session) ID() string { return s.id }

// Provision builds this session's forkable boundary on the HELD connection and
// returns the immutable env-spec the launcher applies. Because the connection is
// held, the matching Teardown later runs under the same identity (Inv 4).
func (s *Session) Provision(ports int, resources []substrate.ResourceReq) (substrate.Wire, error) {
	type resParam struct {
		Name   string `json:"name"`
		Domain string `json:"domain"`
		Ref    string `json:"ref"`
	}
	params := map[string]any{}
	if ports > 0 {
		params["ports"] = ports
	}
	if len(resources) > 0 {
		rs := make([]resParam, 0, len(resources))
		for _, r := range resources {
			rs = append(rs, resParam{Name: r.Name, Domain: r.Domain, Ref: r.Ref})
		}
		params["resources"] = rs
	}
	var spec substrate.Wire
	if err := s.cli.Call("substrate.provision", params, &spec); err != nil {
		return substrate.Wire{}, err
	}
	return spec, nil
}

// MintToken asks the daemon to mint an UNFORGEABLE capability token bound to THIS
// session's connection identity (the daemon takes cc.Session, never a param —
// Inv 4), and returns the token plus the base64 session-liveness lease key. The
// launcher exports the token into the agent env so the cooperative adapter (a
// SEPARATE process) can session.attach and act under the SHARED identity, and
// acquires+renews the session-liveness lease under the returned key. The KEY is
// returned by the daemon so the launcher never FABRICATES it (Inv 9).
func (s *Session) MintToken() (token, livenessKey string, err error) {
	var out struct {
		Token       string `json:"token"`
		LivenessKey string `json:"liveness_key"`
	}
	if err := s.cli.Call("session.mint_token", map[string]any{}, &out); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(out.Token) == "" || strings.TrimSpace(out.LivenessKey) == "" {
		return "", "", fmt.Errorf("daemon returned an empty token or liveness key")
	}
	return out.Token, out.LivenessKey, nil
}

// AcquireSessionLease acquires the session-liveness lease (under the
// daemon-returned base64 livenessKey, holder-bound to THIS session — Inv 4) with
// the given TTL. Its TTL expiry is the ONE TRUE session-death signal liveness
// keys off: while the launcher renews it the session is alive; when the launcher
// stops (clean exit or crash) it lapses and the session is dead. A refused
// acquire (granted=false) is a fail-closed condition for the caller.
func (s *Session) AcquireSessionLease(livenessKey string, ttl time.Duration) error {
	var out struct {
		Granted bool `json:"granted"`
	}
	if err := s.cli.Call("lease.acquire", map[string]any{
		"key":    livenessKey,
		"ttl_ms": ttl.Milliseconds(),
	}, &out); err != nil {
		return err
	}
	if !out.Granted {
		return fmt.Errorf("session-liveness lease was not granted")
	}
	return nil
}

// RenewSessionLease renews the session-liveness lease (holder-bound to THIS
// session). It returns ok=false if the lease is no longer held by this session
// (lapsed/reclaimed) so the caller can stop a wedged renew loop, and an error
// only for a transport failure (which the renew loop tolerates transiently).
func (s *Session) RenewSessionLease(livenessKey string, ttl time.Duration) (ok bool, err error) {
	var out struct {
		OK bool `json:"ok"`
	}
	// BOUNDED: a renew is a fast call, so cap it well below the RPC default so a
	// wedged daemon can't stall the keepalive past the lease TTL / clean-exit budget
	// (a rare false timeout merely triggers a harmless, idempotent re-attach+retry).
	if err := s.callBounded("lease.renew", map[string]any{
		"key":    livenessKey,
		"ttl_ms": ttl.Milliseconds(),
	}, &out, keepaliveCallTimeout); err != nil {
		return false, err
	}
	return out.OK, nil
}

// keepaliveCallTimeout bounds the keepalive's renew/reattach RPCs FAR below the RPC
// client default (120s) so a WEDGED daemon (accepts but never replies) can neither
// defeat re-attach recovery (it must land within the session-liveness lease TTL) nor
// stall clean-exit teardown past cleanExitTimeout. A renew/reattach is a fast call.
const keepaliveCallTimeout = 2 * time.Second

// boundedCaller is the optional per-call-timeout surface; the real *rpcclient.Client
// implements it. A test Conn that only implements Call falls back to the default
// timeout (the keepalive bound matters only against a real, wedgeable daemon).
type boundedCaller interface {
	CallWithReadTimeout(method string, params, out any, d time.Duration) error
}

// callBounded issues a call with a per-call deadline when the underlying connection
// supports it (production), else falls back to the plain Call (test fakes).
func (s *Session) callBounded(method string, params, out any, d time.Duration) error {
	if bc, ok := s.cli.(boundedCaller); ok {
		return bc.CallWithReadTimeout(method, params, out, d)
	}
	return s.cli.Call(method, params, out)
}

// Reattach RE-BINDS this session's connection to its original daemon identity via
// the T2 capability token — the recovery primitive for a DAEMON RESTART (P0 #4).
// When the daemon restarts, the held connection drops; the auto-reconnecting RPC
// client re-dials the new daemon, which mints a FRESH identity for the new
// connection — so a renew then fails (the lease is held by the ORIGINAL id, not
// the new one). Reattach presents the token (which resolves via the DURABLE token
// store the restarted daemon reloaded) so the daemon rebinds this connection back
// to the original id; the very next renew then succeeds and the session-liveness
// lease never lapses, so liveness does not reclaim the still-running session's
// boundary. A non-nil error is either transient (daemon still coming back) or an
// authz fault (the token's session is genuinely dead — its lease expired); the
// keepalive loop tolerates the former and the latter resolves the boundary anyway.
func (s *Session) Reattach(token string) error {
	var out struct {
		Session string `json:"session"`
	}
	// BOUNDED (see keepaliveCallTimeout): a wedged daemon must not block re-attach
	// past the lease TTL / clean-exit budget.
	if err := s.callBounded("session.attach", map[string]any{"token": token}, &out, keepaliveCallTimeout); err != nil {
		return err
	}
	if out.Session != s.id {
		return fmt.Errorf("re-attach bound to %q, expected the original identity %q", out.Session, s.id)
	}
	return nil
}

// ReleaseOwnLeases releases every lease currently held by THIS session — the
// lease half of clean-exit teardown. It releases only leases whose holder is
// this session's identity (lease.release is holder-bound on the daemon side too,
// Inv 4), so it can never free another agent's lock. Idempotent: a lease that
// expired or was already released simply isn't there to release.
//
// SCOPE (C11 CLOSED by T2): in the COOPERATIVE case the agent's leases are taken
// by the adapter, a SEPARATE process — but with T2 the adapter session.attach'es
// the SHARED session identity (the daemon-minted token the launcher exported), so
// its leases are held under s.id and ARE swept here. This includes the
// session-liveness lease the launcher itself holds. The renew goroutine MUST be
// stopped before this runs (Run does so) — otherwise a concurrent renew could
// re-extend the session-liveness lease we are trying to release. A launcher CRASH
// (no clean exit) leaves all of these to liveness's TTL reclaim (project 8): the
// session-liveness lease lapses → the session is declared dead → its boundary +
// any leases held under the shared id are reclaimed.
func (s *Session) ReleaseOwnLeases() error {
	var listed struct {
		Holders []struct {
			Key    string `json:"key"`
			Holder string `json:"holder"`
		} `json:"holders"`
	}
	if err := s.cli.Call("lease.list", nil, &listed); err != nil {
		return fmt.Errorf("list leases: %w", err)
	}
	var firstErr error
	for _, h := range listed.Holders {
		if h.Holder != s.id {
			continue
		}
		if err := s.cli.Call("lease.release", map[string]any{"key": h.Key}, nil); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("release lease: %w", err)
		}
	}
	return firstErr
}

// Teardown removes this session's boundary on the HELD connection (frees ports +
// local-state, removes the worktree) — the substrate half of clean-exit
// teardown. Idempotent on the daemon side (substrate.Teardown no-ops when no
// boundary is live for the session), so a double clean-exit causes no error.
func (s *Session) Teardown() error {
	return s.cli.Call("substrate.teardown", map[string]any{}, nil)
}

// Close drops the held daemon connection. Call after Teardown so the boundary is
// reclaimed before the identity disappears.
func (s *Session) Close() error {
	if s.cli == nil {
		return nil
	}
	return s.cli.Close()
}
