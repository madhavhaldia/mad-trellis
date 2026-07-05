package conformance

import (
	"fmt"
	"os"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/rpcclient"
)

// check_session_reattach.go is the P0 #4 probe: a DAEMON RESTART must not reclaim a
// STILL-LIVE session's boundary.
//
// THE PROPERTY. A launcher holds a session-liveness lease it renews; the lease's
// TTL is the canonical session-death signal liveness keys off. When the daemon
// RESTARTS, the launcher's held connection drops and the reconnecting client gets a
// FRESH daemon identity — so a renew is no longer the lease holder and would lapse.
// The launcher RE-ATTACHES via its capability token (which resolves against the
// daemon's DURABLE token store, reloaded on restart), rebinding to the ORIGINAL
// identity, then renews — so the lease never lapses and liveness leaves the live
// boundary intact.
//
// BLACK BOX. Driven over the PUBLIC surface only (session.whoami / mint_token /
// attach, lease.acquire / renew, substrate.provision, and `mad-trellis recover` to
// force a liveness scan deterministically) — no internal state reads. The
// assertion is OBSERVABLE: the boundary's worktree dir is present (survived) /
// absent (reclaimed) on disk after the restart + a forced liveness scan.
//
// NON-VACUOUS. The Control proves the reclaim is REAL: a session that does NOT
// re-attach (its lease expires) IS reclaimed by the SAME forced scan — so the Run's
// "survived" is meaningful, not a scan that never reclaims anything.

func init() { RegisterCheck(sessionReattachSurvivesRestart{}) }

type sessionReattachSurvivesRestart struct{}

func (sessionReattachSurvivesRestart) ID() string { return "session-reattach-survives-restart" }
func (sessionReattachSurvivesRestart) OwnerProject() string {
	return "session-launcher-shim + session-unifier (P0 #4)"
}
func (sessionReattachSurvivesRestart) Clause() string {
	return "0003 §P0#4 / Inv 4+8: a daemon RESTART must not reclaim a STILL-LIVE session's boundary — the launcher re-attaches via its DURABLE capability token and resumes renewing the session-liveness lease, so liveness leaves the live boundary intact (a session that does NOT re-attach is still reclaimed — the Control)"
}

// reattachSession bundles a held launcher-like session's identity + recovery inputs.
type reattachSession struct {
	id          string
	token       string
	livenessKey string // base64 (daemon-returned — never fabricated)
	worktree    string
}

// setupSession drives the launcher's mint + acquire(session-liveness lease) +
// provision on a HELD connection, returning what a re-attach needs.
func setupSession(c *rpcclient.Client, ttl time.Duration) (reattachSession, error) {
	id, err := whoAmIOn(c)
	if err != nil {
		return reattachSession{}, fmt.Errorf("whoami: %w", err)
	}
	var mint struct {
		Token       string `json:"token"`
		LivenessKey string `json:"liveness_key"`
	}
	if err := c.Call("session.mint_token", map[string]any{}, &mint); err != nil {
		return reattachSession{}, fmt.Errorf("mint_token: %w", err)
	}
	if mint.Token == "" || mint.LivenessKey == "" {
		return reattachSession{}, fmt.Errorf("mint_token returned an empty token/key")
	}
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := c.Call("lease.acquire", map[string]any{"key": mint.LivenessKey, "ttl_ms": ttl.Milliseconds()}, &acq); err != nil {
		return reattachSession{}, fmt.Errorf("acquire: %w", err)
	}
	if !acq.Granted {
		return reattachSession{}, fmt.Errorf("session-liveness lease not granted")
	}
	var b Boundary
	if err := c.Call("substrate.provision", map[string]any{}, &b); err != nil {
		return reattachSession{}, fmt.Errorf("provision: %w", err)
	}
	if b.Cwd == "" {
		return reattachSession{}, fmt.Errorf("provision returned no worktree path")
	}
	return reattachSession{id: id, token: mint.Token, livenessKey: mint.LivenessKey, worktree: b.Cwd}, nil
}

func pathPresent(p string) bool { _, err := os.Stat(p); return err == nil }

func (c sessionReattachSurvivesRestart) Run(s *Scratch) Result {
	conn, err := s.Dial()
	if err != nil {
		return fail(c, "dial: %v", err)
	}
	defer conn.Close()

	// A launcher-like held session with a comfortable TTL (the restart + re-attach
	// must land within it).
	const ttl = 10 * time.Second
	sess, err := setupSession(conn, ttl)
	if err != nil {
		return fail(c, "set up the launcher session: %v", err)
	}
	if !pathPresent(sess.worktree) {
		return fail(c, "precondition: the boundary worktree %q must exist before the restart", sess.worktree)
	}

	// DAEMON RESTART — the durable ledger + token survive; the in-memory session is lost.
	if err := s.RestartDaemon(); err != nil {
		return fail(c, "restart daemon: %v", err)
	}

	// The launcher's #4 recovery: re-attach via the token (resolved from the DURABLE
	// store) — rebinds the reconnected connection to the ORIGINAL identity. The
	// reconnecting RPC client re-dials LAZILY: the first call after the restart hits
	// the dropped connection and errors (then re-dials), so we RETRY — exactly as the
	// launcher's keepalive loop does (it never gives up after a single failure).
	var att struct {
		Session string `json:"session"`
	}
	var attErr error
	for i := 0; i < 25; i++ {
		if attErr = conn.Call("session.attach", map[string]any{"token": sess.token}, &att); attErr == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if attErr != nil {
		return fail(c, "BREACH: re-attach after restart FAILED after retries — the capability token must survive the restart (durable token store): %v", attErr)
	}
	if att.Session != sess.id {
		return fail(c, "re-attach must rebind to the ORIGINAL identity %q; got %q", sess.id, att.Session)
	}
	// Renew as the restored holder — the lease stays live.
	var rn struct {
		OK bool `json:"ok"`
	}
	var rnErr error
	for i := 0; i < 25; i++ {
		if rnErr = conn.Call("lease.renew", map[string]any{"key": sess.livenessKey, "ttl_ms": ttl.Milliseconds()}, &rn); rnErr == nil && rn.OK {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if rnErr != nil || !rn.OK {
		return fail(c, "renew after re-attach must succeed (holder restored): ok=%v err=%v", rn.OK, rnErr)
	}

	// Force a liveness scan: the live (just-renewed) lease must keep the boundary. The
	// scan is right after the renew, well within the TTL, so this is deterministic.
	if rec := s.CLI("recover"); !rec.OK() {
		return fail(c, "force liveness scan (recover): %s", rec.Out())
	}
	if !pathPresent(sess.worktree) {
		return fail(c, "BREACH: a re-attaching (LIVE) session's boundary %q was RECLAIMED across a daemon restart — re-attach failed to keep it alive", sess.worktree)
	}
	return pass(c, "P0 #4: a re-attaching live session's boundary %q SURVIVED a daemon restart — the durable-token re-attach + renew kept the session-liveness lease live, so liveness did not reclaim it (Control proves a non-reattaching session IS reclaimed)", sess.worktree)
}

func (c sessionReattachSurvivesRestart) Control(s *Scratch) error {
	conn, err := s.Dial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// A SHORT TTL so the lease expires quickly when nobody re-attaches/renews.
	sess, err := setupSession(conn, 800*time.Millisecond)
	if err != nil {
		return fmt.Errorf("set up session: %w", err)
	}
	if !pathPresent(sess.worktree) {
		return fmt.Errorf("precondition: worktree %q must exist", sess.worktree)
	}

	if err := s.RestartDaemon(); err != nil {
		return fmt.Errorf("restart daemon: %w", err)
	}
	// Do NOT re-attach. Let the session-liveness lease EXPIRE (TTL measured from the
	// pre-restart acquire), then force a liveness scan.
	time.Sleep(1500 * time.Millisecond)
	if rec := s.CLI("recover"); !rec.OK() {
		return fmt.Errorf("force liveness scan: %s", rec.Out())
	}
	if pathPresent(sess.worktree) {
		return fmt.Errorf("CONTROL VACUOUS: a session that did NOT re-attach (its lease expired) STILL had its boundary %q after a restart + liveness scan — reclaim does not actually fire, so the Run's 'survived' proves nothing", sess.worktree)
	}
	return nil
}
