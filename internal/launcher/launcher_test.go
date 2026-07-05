package launcher

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/substrate"
)

// fakeConn is a scripted daemon connection: it lets the launcher tests drive the
// fail-closed and clean-exit paths deterministically, without a real socket.
type fakeConn struct {
	whoami       string
	whoamiErr    error
	provision    substrate.Wire
	provisionErr error
	teardownErr  error
	teardownN    int
	holders      []map[string]any // lease.list payload
	released     []string         // keys passed to lease.release
	releaseErr   error
	inspect      map[string]any // lease.inspect payload
	inspectErr   error
	closed       bool
	calls        []string

	// T2 session-liveness wiring.
	token         string // session.mint_token returns this (default minted below)
	livenessKey   string // session.mint_token returns this (default below)
	mintErr       error  // session.mint_token fails with this
	acquireGrant  *bool  // lease.acquire granted (nil → true)
	acquireErr    error  // lease.acquire transport error
	acquiredKeys  []string
	renewErr      error // lease.renew transport error
	renewOK       *bool // lease.renew ok (nil → true)
	renewN        int   // count of lease.renew calls
	renewedTTLsMs []int64

	// Conductor (Wing 2) wiring: classify.route returns this trunk lease key. The
	// default "" makes conductor.Converge stop at StatusError BEFORE acquiring any
	// lease or touching git — so a conductor-trigger test stays hermetic (it only
	// proves classify.route was reached, i.e. convergence was ATTEMPTED).
	routeKey string

	mu sync.Mutex // guards the renew-goroutine-touched fields
}

func (f *fakeConn) Call(method string, params any, out any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	switch method {
	case "session.whoami":
		if f.whoamiErr != nil {
			return f.whoamiErr
		}
		return assign(out, map[string]any{"session": f.whoami})
	case "substrate.provision":
		if f.provisionErr != nil {
			return f.provisionErr
		}
		return assign(out, f.provision)
	case "session.mint_token":
		if f.mintErr != nil {
			return f.mintErr
		}
		tok, lk := f.token, f.livenessKey
		if tok == "" {
			tok = "TEST-MINTED-TOKEN"
		}
		if lk == "" {
			lk = base64.StdEncoding.EncodeToString([]byte("mad-substrate:session:v1:" + f.whoami))
		}
		return assign(out, map[string]any{"token": tok, "liveness_key": lk})
	case "substrate.teardown":
		f.teardownN++
		if f.teardownErr != nil {
			return f.teardownErr
		}
		return assign(out, map[string]bool{"ok": true})
	case "lease.acquire":
		if m, ok := params.(map[string]any); ok {
			if k, ok := m["key"].(string); ok {
				f.acquiredKeys = append(f.acquiredKeys, k)
			}
		}
		if f.acquireErr != nil {
			return f.acquireErr
		}
		granted := true
		if f.acquireGrant != nil {
			granted = *f.acquireGrant
		}
		return assign(out, map[string]any{"granted": granted, "holder": f.whoami})
	case "lease.renew":
		f.mu.Lock()
		f.renewN++
		if m, ok := params.(map[string]any); ok {
			if ttl, ok := m["ttl_ms"].(int64); ok {
				f.renewedTTLsMs = append(f.renewedTTLsMs, ttl)
			}
		}
		f.mu.Unlock()
		if f.renewErr != nil {
			return f.renewErr
		}
		ok := true
		if f.renewOK != nil {
			ok = *f.renewOK
		}
		return assign(out, map[string]any{"ok": ok})
	case "lease.list":
		return assign(out, map[string]any{"holders": f.holders})
	case "lease.release":
		if m, ok := params.(map[string]any); ok {
			if k, ok := m["key"].(string); ok {
				f.released = append(f.released, k)
			}
		}
		if f.releaseErr != nil {
			return f.releaseErr
		}
		return assign(out, map[string]bool{"ok": true})
	case "lease.inspect":
		if f.inspectErr != nil {
			return f.inspectErr
		}
		return assign(out, f.inspect)
	case "classify.route":
		// Conductor trunk-lease resolution. An empty key makes Converge return
		// StatusError immediately (no lease.acquire, no git) — hermetic.
		return assign(out, map[string]any{"lease_key": f.routeKey})
	}
	return errors.New("unexpected method: " + method)
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.calls = append(f.calls, "Close") // sentinel so ordering tests can assert Close is last
	return nil
}

// indexOf returns the position of method in the recorded call sequence, or -1.
func indexOf(calls []string, method string) int {
	for i, c := range calls {
		if c == method {
			return i
		}
	}
	return -1
}

// assign marshals v and unmarshals into out — the same JSON round-trip the real
// rpcclient performs, so the fake exercises the launcher's decoding too.
func assign(out any, v any) error {
	if out == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func okSpec() substrate.Wire {
	return substrate.Wire{
		Session: "s-1-abc", Grain: "worktree",
		Cwd: "/tmp/boundary", Branch: "nm/s-1-abc",
		Ports: []int{5000, 5001},
		Env:   map[string]string{"PORT": "5000", "MAD_SESSION": "s-1-abc"},
	}
}

// recordingSpawn is a SpawnFunc that records whether (and how) the agent was run.
type recordingSpawn struct {
	called bool
	target ExecTarget
	cwd    string
	env    map[string]string
	agent  string
	code   int
	err    error
}

func (r *recordingSpawn) fn(target ExecTarget, env map[string]string, agent string, args []string) (int, error) {
	r.called = true
	r.target, r.cwd, r.env, r.agent = target, target.Cwd, env, agent
	return r.code, r.err
}

// --- FAIL-CLOSED (Inv 4): the cardinal property. Every governance failure on the
// path to launch must BLOCK; there is no branch from a failure to running the
// agent ungoverned. The positive control (TestRunLaunchesOnlyWhenGoverned) proves
// these absence-assertions are non-vacuous: spawn IS reached when governance
// fully succeeds, so "spawn not called" is a meaningful refusal, not a dead path.

func TestRunFailsClosedOnDaemonUnreachable(t *testing.T) {
	downDial := func(string) (Conn, error) { return nil, errors.New("connection refused") }
	var sp recordingSpawn
	code, err := Run(Config{Agent: "claude", Dial: downDial, Spawn: sp.fn})

	if sp.called {
		t.Fatal("FAIL-OPEN: the agent was launched despite an unreachable daemon")
	}
	if code != BlockedExitCode {
		t.Fatalf("want BlockedExitCode %d, got %d", BlockedExitCode, code)
	}
	if err == nil {
		t.Fatal("want a BLOCKED error explaining the refusal, got nil")
	}
}

func TestRunFailsClosedOnIdentityUnobtainable(t *testing.T) {
	conn := &fakeConn{whoamiErr: errors.New("authz failed")}
	dial := func(string) (Conn, error) { return conn, nil }
	var sp recordingSpawn
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn})

	if sp.called {
		t.Fatal("FAIL-OPEN: the agent was launched despite an unobtainable identity")
	}
	if code != BlockedExitCode || err == nil {
		t.Fatalf("want BLOCKED, got code=%d err=%v", code, err)
	}
	if !conn.closed {
		t.Error("the held connection should be closed on a failed handshake")
	}
}

// A daemon that answers whoami with an EMPTY session string is a distinct
// fail-closed branch (Open rejects an empty identity) from a whoami ERROR.
func TestRunFailsClosedOnEmptyIdentity(t *testing.T) {
	conn := &fakeConn{whoami: "   "} // whitespace-only ⇒ no usable identity
	dial := func(string) (Conn, error) { return conn, nil }
	var sp recordingSpawn
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn})

	if sp.called {
		t.Fatal("FAIL-OPEN: the agent was launched despite an empty daemon identity")
	}
	if code != BlockedExitCode || err == nil {
		t.Fatalf("want BLOCKED, got code=%d err=%v", code, err)
	}
	if !conn.closed {
		t.Error("the held connection should be closed when the identity is unusable")
	}
}

func TestRunFailsClosedOnProvisionRefused(t *testing.T) {
	conn := &fakeConn{whoami: "s-1-abc", provisionErr: errors.New("substrate: session busy")}
	dial := func(string) (Conn, error) { return conn, nil }
	var sp recordingSpawn
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn})

	if sp.called {
		t.Fatal("FAIL-OPEN: the agent was launched despite a refused provision")
	}
	if code != BlockedExitCode || err == nil {
		t.Fatalf("want BLOCKED, got code=%d err=%v", code, err)
	}
	// No boundary was provisioned (the substrate rolled its partial state back),
	// so the launcher must NOT issue a teardown — there is nothing to reclaim, and
	// a spurious teardown could nuke a same-slug boundary at an idempotent grain.
	if conn.teardownN != 0 {
		t.Errorf("teardown should not run when provision never succeeded; ran %d times", conn.teardownN)
	}
}

// POSITIVE CONTROL for all three fail-closed tests: when the handshake AND
// provision both succeed, spawn IS reached, with the boundary's cwd+env applied.
func TestRunLaunchesOnlyWhenGoverned(t *testing.T) {
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec()}
	dial := func(string) (Conn, error) { return conn, nil }
	sp := &recordingSpawn{code: 0}
	code, err := Run(Config{Agent: "/real/claude", Dial: dial, Spawn: sp.fn})
	if err != nil {
		t.Fatalf("governed launch errored: %v", err)
	}
	if !sp.called {
		t.Fatal("control is vacuous: spawn was never reached even under full governance")
	}
	if sp.cwd != "/tmp/boundary" {
		t.Errorf("agent cwd = %q, want the boundary cwd /tmp/boundary", sp.cwd)
	}
	if sp.env["PORT"] != "5000" {
		t.Errorf("env-spec not applied to the agent: PORT=%q", sp.env["PORT"])
	}
	if sp.agent != "/real/claude" {
		t.Errorf("agent = %q, want the resolved binary", sp.agent)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// --- CLEAN-EXIT TEARDOWN (Inv 4 symmetric counterpart; closes chafe C6): on
// NORMAL exit the launcher releases ITS OWN session's leases and tears the
// boundary down, idempotently, with zero orphans.

func TestCleanExitTeardownReleasesOwnLeasesAndBoundary(t *testing.T) {
	mineKey := base64.StdEncoding.EncodeToString([]byte("mad-substrate:trunk:v1"))
	othersKey := base64.StdEncoding.EncodeToString([]byte("someone-elses-lock"))
	conn := &fakeConn{
		whoami:    "s-1-abc",
		provision: okSpec(),
		holders: []map[string]any{
			{"key": mineKey, "holder": "s-1-abc"},     // mine → must be released
			{"key": othersKey, "holder": "s-2-other"}, // another session → must NOT be touched
		},
	}
	dial := func(string) (Conn, error) { return conn, nil }
	sp := &recordingSpawn{code: 0}
	if _, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if conn.teardownN != 1 {
		t.Errorf("substrate.teardown should run exactly once on clean exit; ran %d", conn.teardownN)
	}
	if len(conn.released) != 1 || conn.released[0] != mineKey {
		t.Errorf("clean-exit should release ONLY this session's lease; released=%v", conn.released)
	}
	if !conn.closed {
		t.Error("the held connection should be closed after teardown")
	}
	// Ordering invariant: release the lease, THEN tear the boundary down, THEN
	// close the connection (so the boundary is reclaimed before the identity
	// disappears). The connection is the session — closing it first would strand
	// the in-daemon boundary keyed to that identity.
	rel := indexOf(conn.calls, "lease.release")
	tear := indexOf(conn.calls, "substrate.teardown")
	closed := indexOf(conn.calls, "Close")
	if !(rel >= 0 && tear >= 0 && closed >= 0 && rel < tear && tear < closed) {
		t.Errorf("clean-exit order must be release < teardown < close; calls=%v", conn.calls)
	}
}

func TestCleanExitTeardownRunsEvenWhenAgentFails(t *testing.T) {
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec()}
	dial := func(string) (Conn, error) { return conn, nil }
	sp := &recordingSpawn{code: 3, err: nil} // agent exited non-zero
	code, _ := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn})

	if code != 3 {
		t.Errorf("agent exit code %d should propagate, got %d", 3, code)
	}
	if conn.teardownN != 1 {
		t.Errorf("teardown must still run after a non-zero agent exit (zero orphans); ran %d", conn.teardownN)
	}
}

// --- T2 SESSION-LIVENESS WIRING ---------------------------------------------

// The governed launch mints a session token, acquires the session-liveness lease
// under the DAEMON-RETURNED key (Inv 9), and exports MAD_SESSION_TOKEN into
// the agent env (alongside MAD_SESSION) so the cooperative adapter can
// session.attach the SHARED identity.
func TestRunMintsAcquiresAndExportsSessionToken(t *testing.T) {
	wantKey := base64.StdEncoding.EncodeToString([]byte("mad-substrate:session:v1:s-1-abc"))
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec(), token: "TOK-XYZ", livenessKey: wantKey}
	dial := func(string) (Conn, error) { return conn, nil }
	sp := &recordingSpawn{code: 0}
	if _, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sp.called {
		t.Fatal("vacuous: spawn not reached under full governance")
	}
	if sp.env["MAD_SESSION_TOKEN"] != "TOK-XYZ" {
		t.Fatalf("agent env must carry the minted token; got %q", sp.env["MAD_SESSION_TOKEN"])
	}
	if sp.env["MAD_SESSION"] != "s-1-abc" {
		t.Fatalf("agent env must still carry MAD_SESSION; got %q", sp.env["MAD_SESSION"])
	}
	if sp.env["MAD_LAUNCHED"] != "1" {
		t.Fatalf("launcher-spawned agents must carry MAD_LAUNCHED=1; got %q", sp.env["MAD_LAUNCHED"])
	}
	// The session-liveness lease was acquired under the DAEMON-RETURNED key (Inv 9).
	if len(conn.acquiredKeys) != 1 || conn.acquiredKeys[0] != wantKey {
		t.Fatalf("session-liveness lease must be acquired under the daemon-returned key %q; got %v", wantKey, conn.acquiredKeys)
	}
	// mint precedes acquire precedes spawn.
	mi := indexOf(conn.calls, "session.mint_token")
	ai := indexOf(conn.calls, "lease.acquire")
	if !(mi >= 0 && ai >= 0 && mi < ai) {
		t.Fatalf("mint must precede session-liveness acquire; calls=%v", conn.calls)
	}
}

// A failed mint or refused session-liveness acquire is FAIL-CLOSED (Inv 4): the
// agent must NOT run.
func TestRunFailsClosedOnMintTokenError(t *testing.T) {
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec(), mintErr: errors.New("mint refused")}
	dial := func(string) (Conn, error) { return conn, nil }
	var sp recordingSpawn
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn})
	if sp.called {
		t.Fatal("FAIL-OPEN: the agent ran despite a failed session-token mint")
	}
	if code != BlockedExitCode || err == nil {
		t.Fatalf("want BLOCKED, got code=%d err=%v", code, err)
	}
}

func TestRunFailsClosedOnSessionLeaseRefused(t *testing.T) {
	no := false
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec(), acquireGrant: &no}
	dial := func(string) (Conn, error) { return conn, nil }
	var sp recordingSpawn
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn})
	if sp.called {
		t.Fatal("FAIL-OPEN: the agent ran despite a refused session-liveness lease")
	}
	if code != BlockedExitCode || err == nil {
		t.Fatalf("want BLOCKED, got code=%d err=%v", code, err)
	}
}

// A fail-closed mint/acquire AFTER provision must NOT leak the boundary: the
// teardown defer is registered immediately after provision, so the provisioned
// boundary is reclaimed even though the launch is blocked. (Regression guard for
// the no-leak ordering.)
func TestRunFailsClosedAfterProvisionStillTearsDownBoundary(t *testing.T) {
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec(), mintErr: errors.New("mint refused")}
	dial := func(string) (Conn, error) { return conn, nil }
	var sp recordingSpawn
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn})
	if sp.called {
		t.Fatal("FAIL-OPEN: the agent ran despite a failed mint")
	}
	if code != BlockedExitCode || err == nil {
		t.Fatalf("want BLOCKED, got code=%d err=%v", code, err)
	}
	if conn.teardownN != 1 {
		t.Errorf("a fail-closed return after provision must still tear down the boundary; teardownN=%d", conn.teardownN)
	}
	if !conn.closed {
		t.Error("the held connection should be closed after the blocked launch")
	}
}

// The renew goroutine renews the session-liveness lease at ~TTL/2 while the agent
// runs, and is stopped on clean exit. We block the (fake) spawn until the renew
// loop has fired at least once, then release it; the deferred clean-exit must
// stop the loop and release the lease.
func TestRunRenewsSessionLeaseAndStopsOnExit(t *testing.T) {
	livenessKey := base64.StdEncoding.EncodeToString([]byte("mad-substrate:session:v1:s-1-abc"))
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec(), livenessKey: livenessKey}
	dial := func(string) (Conn, error) { return conn, nil }

	// Spawn blocks until at least one renew has been observed, then returns. This
	// keeps the agent "alive" long enough for the ~TTL/2 ticker to fire WITHOUT a
	// fixed sleep-for-luck.
	const testTTL = 20 * time.Millisecond // renew ticker at ~10ms keeps the test fast
	renewed := make(chan struct{})
	var once sync.Once
	spawn := func(target ExecTarget, env map[string]string, agent string, args []string) (int, error) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			conn.mu.Lock()
			n := conn.renewN
			conn.mu.Unlock()
			if n >= 1 {
				once.Do(func() { close(renewed) })
				return 0, nil
			}
			time.Sleep(2 * time.Millisecond)
		}
		return 0, errors.New("renew never fired within deadline")
	}
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: spawn, sessionLeaseTTL: testTTL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	select {
	case <-renewed:
	default:
		t.Fatal("renew goroutine never renewed the session-liveness lease")
	}
	// Every renew used the configured TTL.
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.renewN < 1 {
		t.Fatalf("expected >=1 renew, got %d", conn.renewN)
	}
	for _, ttl := range conn.renewedTTLsMs {
		if ttl != testTTL.Milliseconds() {
			t.Fatalf("renew TTL = %dms, want %dms", ttl, testTTL.Milliseconds())
		}
	}
	// After clean exit the session-liveness lease was released (it was held by
	// s-1-abc and is now swept). The lease.list returned no holders here, so the
	// real release path is exercised in the shared-id sweep test below; here we just
	// assert the renew loop was bounded (Run returned, so renewDone was joined).
}

// C11: a lease held under the SHARED session id by a SEPARATE connection (the
// adapter model: it session.attach'ed the launcher's identity, so its lease holder
// IS s.id) is swept by the launcher's clean-exit ReleaseOwnLeases. This is the
// chafe C11 closure — the adapter's leases were previously orphaned until TTL.
func TestCleanExitSweepsSharedIdLeaseFromSeparateConnection(t *testing.T) {
	sharedID := "s-1-abc"
	livenessKey := base64.StdEncoding.EncodeToString([]byte("mad-substrate:session:v1:" + sharedID))
	adapterKey := base64.StdEncoding.EncodeToString([]byte("mad-substrate:singular:v1:db")) // taken by the attached adapter
	otherKey := base64.StdEncoding.EncodeToString([]byte("mad-substrate:singular:v1:other"))
	conn := &fakeConn{
		whoami:      sharedID,
		provision:   okSpec(),
		livenessKey: livenessKey,
		holders: []map[string]any{
			// The session-liveness lease the launcher itself holds.
			{"key": livenessKey, "holder": sharedID},
			// A lease the ADAPTER took under the SHARED id (it attached) — must be swept.
			{"key": adapterKey, "holder": sharedID},
			// A truly foreign session's lease — must NOT be touched.
			{"key": otherKey, "holder": "s-2-foreign"},
		},
	}
	dial := func(string) (Conn, error) { return conn, nil }
	sp := &recordingSpawn{code: 0}
	if _, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	released := map[string]bool{}
	for _, k := range conn.released {
		released[k] = true
	}
	if !released[livenessKey] {
		t.Errorf("clean-exit must release the session-liveness lease; released=%v", conn.released)
	}
	if !released[adapterKey] {
		t.Errorf("C11: clean-exit must sweep the adapter's lease held under the SHARED id; released=%v", conn.released)
	}
	if released[otherKey] {
		t.Errorf("clean-exit must NOT touch a foreign session's lease; released=%v", conn.released)
	}
	if len(conn.released) != 2 {
		t.Errorf("expected exactly 2 releases (liveness + adapter), got %v", conn.released)
	}
}
