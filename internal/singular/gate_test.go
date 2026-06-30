package singular

// Hand-authored invariant suite for project 7 (singular-gate) — the contract,
// authored by hand (REVIEW-GATED). Each owned clause → a proving test, every
// absence-assertion carries a positive control:
//
//   Inv 8 (c) default-deny ground state → TestDefaultDenyGroundState (+control)
//   unknown/malformed → DENY            → TestUnknownGrantDenies
//   classify-upward under ambiguity     → TestSilentResourceDenied
//   serialized supervised exclusivity   → TestSupervisedExclusivity (+real ledger)
//   proxy-bypass / uncooperative-agent  → TestNoRealEndpointForNonSupervised (+control)
//   2(b) deterministic, no inference    → TestResolveDeterministic (+control)
//   Inv 3 dead-holder grant reclaim     → TestSupervisedGrantTTLReclaim
//   audit completeness                  → TestAuditEmitted

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/lease"
	"github.com/madhavhaldia/mad-substrate/internal/manifest"
)

// --- helpers -----------------------------------------------------------------

// clsWith returns a real classifier where every name in forkable is Forkable and
// everything else external is Singular (the default classify-upward). Using the
// REAL classifier (the soul) keeps the gate honest about consuming it.
func clsWith(forkable ...string) *manifest.Classifier {
	m := manifest.DefaultManifest()
	for _, f := range forkable {
		m.ForkableResources[f] = true
	}
	return manifest.New(m)
}

type fakeLease struct {
	mu   sync.Mutex
	held map[string]string
}

func newFakeLease() *fakeLease { return &fakeLease{held: map[string]string{}} }

func (f *fakeLease) Acquire(key []byte, holder string, _ time.Duration) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := string(key)
	if h, ok := f.held[k]; ok && h != holder {
		return false, h, nil
	}
	f.held[k] = holder
	return true, holder, nil
}

func (f *fakeLease) Renew(key []byte, holder string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[string(key)] == holder, nil
}

func (f *fakeLease) Release(key []byte, holder string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := string(key)
	if f.held[k] == holder {
		delete(f.held, k)
		return true, nil
	}
	return false, nil
}

func mustGate(t *testing.T, opts Options) *Gate {
	t.Helper()
	if opts.Leases == nil {
		opts.Leases = newFakeLease()
	}
	g, err := New(opts)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}
	return g
}

// --- tests -------------------------------------------------------------------

// [Inv 8 (c)] DEFAULT-DENY GROUND STATE — the load-bearing test. A declared
// singular resource with NO grant routes to NO reachable real endpoint and the
// access is denied. +control: a mock-granted resource IS granted, proving deny is
// not vacuous.
func TestDefaultDenyGroundState(t *testing.T) {
	real := "https://api.stripe.com/secret"
	g := mustGate(t, Options{
		Classifier: clsWith(),
		Grants:     map[string]manifest.Grant{"mocked": {Mode: "mock", Endpoint: real}},
	})

	// No grant → deny.
	a, err := g.Request("s-1", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode != Deny || a.Granted || a.RealReachable {
		t.Fatalf("an ungranted singular resource must be DENIED; got %+v", a)
	}
	for _, v := range a.Env {
		if !strings.HasPrefix(v, "mad-substrate-denied://") {
			t.Fatalf("denied env must route to a non-routable deny sentinel; got %q", v)
		}
	}

	// +control: a mock grant IS granted (deny is not vacuous) — but still no real side effect.
	c, _ := g.Request("s-1", "mocked")
	if c.Mode != Mock || !c.Granted {
		t.Fatalf("control: a mock grant must be granted; got %+v", c)
	}
	if c.RealReachable {
		t.Fatal("a mock grant must not reach the real endpoint")
	}
}

// unknown / malformed / empty grant mode → DENY (Inv 9 classify-upward under
// ambiguity; the ground state catches every non-explicit case).
func TestUnknownGrantDenies(t *testing.T) {
	for _, mode := range []string{"", "real", "MOCK!", "allow", "passthrough", "  ", "supervis"} {
		g := mustGate(t, Options{
			Classifier: clsWith(),
			Grants:     map[string]manifest.Grant{"x": {Mode: mode, Endpoint: "https://real"}},
		})
		r := g.Resolve("x")
		if r.Mode != Deny {
			t.Fatalf("unknown grant mode %q must resolve DENY; got %s", mode, r.Mode)
		}
		a, _ := g.Request("s-1", "x")
		if a.RealReachable {
			t.Fatalf("unknown grant mode %q must not reach the real endpoint", mode)
		}
	}
}

// A silent (undeclared) external resource classifies upward to singular and, with
// no grant, is denied — and a NON-singular (forkable) resource is not the gate's
// concern (Singular=false), so the gate never gates a forkable resource.
func TestSilentResourceDenied(t *testing.T) {
	g := mustGate(t, Options{Classifier: clsWith("cache")})
	if r := g.Resolve("undeclared-saas"); !r.Singular || r.Mode != Deny {
		t.Fatalf("a silent external resource must be singular+deny; got %+v", r)
	}
	if r := g.Resolve("cache"); r.Singular {
		t.Fatalf("a forkable resource must NOT be the gate's concern; got %+v", r)
	}
}

// [serialized] ≤1 supervised holder per resource (CAS-fail-fast), proven against
// the REAL ledger. The second concurrent supervised request is denied (no real
// endpoint); after the holder releases, it can be granted.
func TestSupervisedExclusivity(t *testing.T) {
	real := "postgres://prod-db/main"
	l, err := lease.Open("", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	g := mustGate(t, Options{
		Classifier: clsWith(),
		Grants:     map[string]manifest.Grant{"db": {Mode: "supervised", Endpoint: real}},
		Leases:     realLeaseGate{l},
	})

	a, _ := g.Request("s-A", "db")
	if !a.Granted || !a.RealReachable || a.Env[envName("db")] != real {
		t.Fatalf("first supervised request must be granted to the real endpoint; got %+v", a)
	}
	b, _ := g.Request("s-B", "db")
	if b.Granted || b.RealReachable {
		t.Fatalf("a SECOND concurrent supervised holder must be denied; got %+v", b)
	}
	if b.Env[envName("db")] == real {
		t.Fatal("the denied second holder must NOT receive the real endpoint")
	}
	// Holder releases → the grant becomes available again.
	if ok, _ := g.ReleaseSupervised("s-A", "db"); !ok {
		t.Fatal("the holder must release its own grant")
	}
	c, _ := g.Request("s-B", "db")
	if !c.Granted || !c.RealReachable {
		t.Fatalf("after release the next requester must be granted; got %+v", c)
	}
}

// [proxy-bypass / Inv 4 uncooperative-agent] for deny/mock/proxy the env-spec
// NEVER carries the real endpoint, so it is unreachable from the env-spec alone.
// +control: supervised-granted DOES expose the real endpoint, proving the absence
// check is meaningful (not "endpoint always absent").
func TestNoRealEndpointForNonSupervised(t *testing.T) {
	real := "https://real.endpoint/secret"
	for _, mode := range []string{"", "mock", "proxy"} {
		g := mustGate(t, Options{
			Classifier: clsWith(),
			Grants:     map[string]manifest.Grant{"r": {Mode: mode, Endpoint: real}},
		})
		a, _ := g.Request("s-1", "r")
		for k, v := range a.Env {
			if strings.Contains(v, real) {
				t.Fatalf("mode %q LEAKED the real endpoint via %s=%q (proxy-bypass)", mode, k, v)
			}
		}
		if a.RealReachable {
			t.Fatalf("mode %q must not be real-reachable", mode)
		}
	}
	// +control: supervised IS real-reachable (the absence check above is non-vacuous).
	gs := mustGate(t, Options{
		Classifier: clsWith(),
		Grants:     map[string]manifest.Grant{"r": {Mode: "supervised", Endpoint: real}},
	})
	a, _ := gs.Request("s-1", "r")
	if !a.RealReachable || a.Env[envName("r")] != real {
		t.Fatalf("control: a supervised grant must expose the real endpoint; got %+v", a)
	}
}

// [2b] Resolve is a deterministic, no-inference function. +control: a
// nondeterministic classifier makes the gate's verdict vary, proving the
// determinism assertion is non-vacuous (an LLM/heuristic verdict source would
// fail it). The cross-project static no-LLM check is owned by 10a.
func TestResolveDeterministic(t *testing.T) {
	g := mustGate(t, Options{Classifier: clsWith(), Grants: map[string]manifest.Grant{"x": {Mode: "proxy"}}})
	first := g.Resolve("x")
	for i := 0; i < 8; i++ {
		if g.Resolve("x") != first {
			t.Fatal("Resolve must be deterministic")
		}
	}
	// +control: a flipping classifier yields a varying verdict.
	flip := &flipClassifier{}
	gc := mustGate(t, Options{Classifier: flip})
	a := gc.Resolve("x").Singular
	b := gc.Resolve("x").Singular
	if a == b {
		t.Fatal("control: the flipping classifier should vary (non-vacuity check)")
	}
}

type flipClassifier struct{ n int }

func (f *flipClassifier) Classify(manifest.ResourceRef) manifest.Kind {
	f.n++
	if f.n%2 == 0 {
		return manifest.Singular
	}
	return manifest.Forkable
}

// [Inv 3] a supervised grant held by a DEAD holder is reclaimable after its lease
// TTL — the auto-release path liveness relies on. Proven with a real ledger + an
// injected clock: advance past the TTL and a different session acquires.
func TestSupervisedGrantTTLReclaim(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
	l, err := lease.Open("", clk)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	g := mustGate(t, Options{
		Classifier: clsWith(),
		Grants:     map[string]manifest.Grant{"db": {Mode: "supervised", Endpoint: "postgres://real"}},
		Leases:     realLeaseGate{l},
		GrantTTLMs: 100,
	})
	a, _ := g.Request("dead-holder", "db")
	if !a.Granted {
		t.Fatal("first supervised request must be granted")
	}
	// Holder "dies" without releasing; before TTL another session is still denied.
	if b, _ := g.Request("s-B", "db"); b.Granted {
		t.Fatal("before the TTL elapses, the grant must still be exclusive")
	}
	clk.advance(250 * time.Millisecond) // past the 100ms TTL
	c, _ := g.Request("s-B", "db")
	if !c.Granted || !c.RealReachable {
		t.Fatalf("after the TTL the dead holder's grant must be reclaimable; got %+v", c)
	}
}

// [audit] EVERY grant decision is emitted through the audit interface — deny,
// mock, proxy, and supervised-granted all produce a record (non-vacuous: each
// distinct path is exercised).
func TestAuditEmitted(t *testing.T) {
	var mu sync.Mutex
	var kinds []string
	spy := func(_, kind string, _ []byte) { mu.Lock(); kinds = append(kinds, kind); mu.Unlock() }
	g := mustGate(t, Options{
		Classifier: clsWith(),
		Grants: map[string]manifest.Grant{
			"db": {Mode: "supervised", Endpoint: "x"},
			"m":  {Mode: "mock"},
			"p":  {Mode: "proxy"},
		},
		Audit: spy,
	})
	g.Request("s-1", "ungranted") // → singular.denied
	g.Request("s-1", "db")        // → singular.granted (supervised)
	g.Request("s-1", "m")         // → singular.granted (mock)
	g.Request("s-1", "p")         // → singular.granted (proxy)
	mu.Lock()
	defer mu.Unlock()
	granted, denied := 0, 0
	for _, k := range kinds {
		switch k {
		case "singular.granted":
			granted++
		case "singular.denied":
			denied++
		}
	}
	if denied < 1 || granted < 3 {
		t.Fatalf("every decision must be audited (>=1 denied, >=3 granted: mock/proxy/supervised); got %v", kinds)
	}
}

// [Inv 9 / Inv 8(c)] a supervised grant declared with NO endpoint is MALFORMED
// and must resolve to the ground-state DENY — it must NOT be granted, must NOT
// expose a bare value, and must NOT consume the exclusive supervised lease.
func TestEmptyEndpointSupervisedDenies(t *testing.T) {
	lg := newFakeLease()
	g := mustGate(t, Options{
		Classifier: clsWith(),
		Grants:     map[string]manifest.Grant{"db": {Mode: "supervised", Endpoint: "   "}},
		Leases:     lg,
	})
	if r := g.Resolve("db"); r.Mode != Deny {
		t.Fatalf("a supervised grant with no endpoint must resolve DENY; got %s", r.Mode)
	}
	a, _ := g.Request("s-1", "db")
	if a.Granted || a.RealReachable {
		t.Fatalf("an endpoint-less supervised grant must be denied; got %+v", a)
	}
	if v := a.Env[envName("db")]; !strings.HasPrefix(v, "mad-substrate-denied://") {
		t.Fatalf("the denied env must be a non-routable sentinel, not a bare value; got %q", v)
	}
	// The malformed grant must NOT have taken the exclusive lease.
	lg.mu.Lock()
	_, taken := lg.held[string(g.key("db"))]
	lg.mu.Unlock()
	if taken {
		t.Fatal("a denied (malformed) supervised grant must NOT consume the lease")
	}
}

// [serialized] a LIVE supervised holder keeps exclusivity by RENEWING; the
// same-holder re-request renews and re-fetches the real endpoint (not a self-
// deny), while a DIFFERENT session stays denied. Proven against the real ledger.
func TestSupervisedRenewKeepsExclusivity(t *testing.T) {
	real := "postgres://prod/main"
	l, err := lease.Open("", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	g := mustGate(t, Options{
		Classifier: clsWith(),
		Grants:     map[string]manifest.Grant{"db": {Mode: "supervised", Endpoint: real}},
		Leases:     realLeaseGate{l},
	})
	a, _ := g.Request("s-A", "db")
	if !a.Granted || a.Env[envName("db")] != real {
		t.Fatalf("first supervised request must grant the real endpoint; got %+v", a)
	}
	// The SAME holder re-requesting gets a renew + the real endpoint (not a deny).
	again, _ := g.Request("s-A", "db")
	if !again.Granted || !again.RealReachable || again.Env[envName("db")] != real {
		t.Fatalf("same-holder re-request must renew + re-fetch the real endpoint; got %+v", again)
	}
	// An explicit renew by the holder succeeds; by a non-holder fails.
	if ok, _ := g.Renew("s-A", "db"); !ok {
		t.Fatal("the holder must be able to renew its grant")
	}
	if ok, _ := g.Renew("s-B", "db"); ok {
		t.Fatal("a non-holder must NOT be able to renew another's grant")
	}
	// A DIFFERENT session is still denied while s-A holds it.
	b, _ := g.Request("s-B", "db")
	if b.Granted || b.RealReachable {
		t.Fatalf("a different session must be denied while the grant is held; got %+v", b)
	}
}

// [C16 — T2 composition] a SUPERVISED grant taken by the cooperative adapter is
// holder-bound to cc.Session (Inv 4). With T2 the adapter session.attach'es the
// SHARED session identity, so its grant's holder IS the launcher's session id.
// This test proves the two T2 reclaim paths COMPOSE over a supervised grant held
// under the shared id:
//
//	(a) CLEAN-EXIT SWEEP: the launcher's ReleaseOwnLeases releases every lease held
//	    by the shared id — modeled here by releasing the supervised grant under that
//	    id (holder-bound). The grant frees and a fresh session can acquire it.
//	(b) SESSION-DEATH RECLAIM: a supervised grant under the shared id auto-releases
//	    on the lease TTL (the path liveness reclaims on session death) — modeled by
//	    advancing past the TTL without a renew; a fresh session then acquires it.
//
// Minimal code change: the gate already binds supervised grants to cc.Session and
// auto-releases on the lease TTL; T2 only changes WHICH id that is (the shared
// one). This test pins that the composition holds.
func TestSupervisedGrantUnderSharedSessionSweptAndReclaimed(t *testing.T) {
	const real = "postgres://prod/main"
	const sharedID = "s-1-shared" // the launcher's session; the adapter attaches to it

	// (a) CLEAN-EXIT SWEEP: a release by the shared id (what ReleaseOwnLeases does)
	// frees the grant.
	t.Run("clean_exit_sweep", func(t *testing.T) {
		l, err := lease.Open("", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		g := mustGate(t, Options{
			Classifier: clsWith(),
			Grants:     map[string]manifest.Grant{"db": {Mode: "supervised", Endpoint: real}},
			Leases:     realLeaseGate{l},
		})
		// The adapter (under the SHARED id) takes the supervised grant.
		a, _ := g.Request(sharedID, "db")
		if !a.Granted || !a.RealReachable {
			t.Fatalf("adapter's supervised request under the shared id must be granted; got %+v", a)
		}
		// A different session is denied while it is held (exclusivity intact).
		if b, _ := g.Request("s-2-other", "db"); b.Granted {
			t.Fatal("the grant must be exclusive while held under the shared id")
		}
		// Clean-exit sweep: ReleaseOwnLeases releases every lease held by the shared
		// id — including this supervised grant (holder-bound release).
		if ok, _ := g.ReleaseSupervised(sharedID, "db"); !ok {
			t.Fatal("the supervised grant under the shared id must be releasable by that id")
		}
		// Progress: a fresh session can now acquire the swept grant.
		if c, _ := g.Request("s-3-fresh", "db"); !c.Granted || !c.RealReachable {
			t.Fatalf("after the clean-exit sweep a fresh session must acquire the grant; got %+v", c)
		}
	})

	// (b) SESSION-DEATH RECLAIM: with no renew, the grant lapses on its TTL — the
	// path liveness uses to reclaim a dead session's grant — and is re-acquirable.
	t.Run("session_death_ttl_reclaim", func(t *testing.T) {
		clk := &manualClock{t: time.Unix(1_700_000_000, 0)}
		l, err := lease.Open("", clk)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		g := mustGate(t, Options{
			Classifier: clsWith(),
			Grants:     map[string]manifest.Grant{"db": {Mode: "supervised", Endpoint: real}},
			Leases:     realLeaseGate{l},
			GrantTTLMs: 100,
		})
		if a, _ := g.Request(sharedID, "db"); !a.Granted {
			t.Fatal("adapter's supervised request must be granted")
		}
		// The whole session dies (launcher + adapter): no one renews. Before the TTL
		// the grant is still exclusive.
		if b, _ := g.Request("s-4-other", "db"); b.Granted {
			t.Fatal("before the TTL the dead session's grant must stay exclusive")
		}
		clk.advance(250 * time.Millisecond) // past the 100ms TTL (the session is dead)
		if c, _ := g.Request("s-5-fresh", "db"); !c.Granted || !c.RealReachable {
			t.Fatalf("after the TTL a dead session's grant must be reclaimable; got %+v", c)
		}
	})
}

// --- real-ledger + clock helpers ---------------------------------------------

type realLeaseGate struct{ l *lease.Ledger }

func (g realLeaseGate) Acquire(key []byte, holder string, ttl time.Duration) (bool, string, error) {
	res, err := g.l.Acquire(key, holder, ttl)
	if err != nil {
		return false, "", err
	}
	return res.Granted, res.Holder, nil
}

func (g realLeaseGate) Renew(key []byte, holder string, ttl time.Duration) (bool, error) {
	res, err := g.l.Renew(key, holder, ttl)
	if err != nil {
		return false, err
	}
	return res.OK, nil
}

func (g realLeaseGate) Release(key []byte, holder string) (bool, error) {
	return g.l.Release(key, holder)
}

type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
