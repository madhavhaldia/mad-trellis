package conformance

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// check_escape.go is the ESCAPE-RESISTANCE adversarial conjunct over ALL THREE
// surfaces (docs/0003 §10a "the escape-resistance adversarial conjunct over ALL
// THREE surfaces ... worktree-FS escape, mediated-remote bypass, singular-proxy
// bypass"; GROUNDING Inv 4). The probe acts as an UNCOOPERATIVE agent: it calls
// NO adapter and obeys no cooperative protocol — it directly attempts the escape
// with RAW git / a raw filesystem write / only its env-spec sentinel, and the
// SUBSTRATE / INTEGRATOR / GATE must refuse it STRUCTURALLY. Safety must NOT
// depend on the agent cooperating.
//
// Each escape is one check (so a single surface's regression names itself), and
// each carries a positive control that genuinely flips RED:
//
//	(i)   worktree-FS escape  — a raw write outside the boundary cannot corrupt a
//	      sibling's in-progress state or the trunk. CONTROL: a write INSIDE the
//	      boundary IS visible (so the "did-not-corrupt" verdict is non-vacuous).
//	(ii)  mediated-remote bypass — a raw `git push` to the trunk ref in the bare
//	      mediated repo is STRUCTURALLY rejected by the update hook and the trunk
//	      ref is unchanged. CONTROL: the integrator's legit promote DOES advance
//	      the trunk (so the rejection is not a dead remote).
//	(iii) singular-proxy bypass — an agent holding only a deny/mock/proxy env-spec
//	      sentinel cannot reach the real endpoint (the sentinel is non-routable, no
//	      real side effect). CONTROL: a SUPERVISED grant DOES expose a real
//	      reachable endpoint (so "no real endpoint" is non-vacuous — the gate can
//	      hand a real endpoint, it just refuses to for deny/mock/proxy).

func init() {
	RegisterCheck(escapeWorktreeFS{})
	RegisterCheck(escapeMediatedRemote{})
	RegisterCheck(escapeSingularProxy{})
}

// ----------------------------------------------------------------------------
// (i) worktree-FS escape — a raw write outside the boundary cannot corrupt a
// sibling's in-progress state or the trunk.
// ----------------------------------------------------------------------------

type escapeWorktreeFS struct{}

func (escapeWorktreeFS) ID() string           { return "escape-worktree-fs" }
func (escapeWorktreeFS) OwnerProject() string { return "isolation-substrate (escape-resistance)" }
func (escapeWorktreeFS) Clause() string {
	return "escape (i): at the worktree grain, the substrate provisions DISJOINT boundaries + refuses to hand out an escaping path; the uncooperative agent cannot reach trunk (Inv 1/4)"
}

// GRAIN SCOPE — read honestly. At the v1 WORKTREE grain a plain directory cannot
// structurally confine a process that runs arbitrary shell (`cd /` + write
// elsewhere): raw co-located ABSOLUTE-path FS writes to a known sibling are
// confined only at the CONTAINER/VM grain (chafe C5; Inv 10-grainswap; Inv 4
// "sandboxed harder"). What the WORKTREE grain DOES guarantee, and what this probe
// asserts NON-VACUOUSLY, is threefold:
//
//	(a) DISJOINTNESS — two provisioned boundaries get NON-NESTED, distinct
//	    cwd/branch/port-block/state-dir, and neither is handed a path into the other
//	    (no shared writable forkable path = no FS coordination channel);
//	(b) NO ESCAPING PATH HANDED OUT — when driven with a genuinely uncooperative
//	    path-traversal/symlink resource ref, the substrate's Contain-style resolution
//	    refuses to hand back a path that escapes the boundary (it sanitizes/contains
//	    it INSIDE the agent's own state root, never to /etc or a sibling); AND
//	(c) THE CONVERGENT FLOOR — the grain-INDEPENDENT hard guarantee: an uncooperative
//	    agent with only worktree FS access cannot advance or observe TRUNK.
//
// It does NOT assert full FS confinement (a guarantee v1 does not provide at this
// grain) — that would be a false green or a perpetual red.

func (c escapeWorktreeFS) Run(s *Scratch) Result {
	// Two boundaries: the uncooperative attacker (a1) and the victim (a2). We use the
	// FULL provision Wire (env + state_dirs), not just the spawn cwd, so we can assert
	// disjointness over EVERY provisioned dimension.
	a1, err := s.Provision()
	if err != nil {
		return fail(c, "provision attacker boundary: %v", err)
	}
	a2, err := s.Provision()
	if err != nil {
		return fail(c, "provision victim boundary: %v", err)
	}

	// (a) DISJOINTNESS the grain guarantees: distinct, non-nested cwd/branch/ports/
	// state-dir, and neither boundary handed a path into the other.
	if r := assertBoundariesDisjoint(c, a1, a2); !r.Pass {
		return r
	}

	// (b) NO ESCAPING PATH HANDED OUT: drive the substrate with genuinely uncooperative
	// path-traversal AND symlink-shaped resource refs, then assert EXHAUSTIVELY that every
	// routed path the substrate handed back is CONTAINED within this boundary's own roots.
	// The breach is ANY routed path that escapes the boundary, WHEREVER it lands — not just
	// /etc or a sibling (that narrowing was the vacuity hole the sign-off caught). This
	// asserts Contain's real, load-bearing guarantee: mad-substrate never HANDS an agent a path
	// that escapes its boundary.
	escaper, err := s.Provision(
		provisionResource{Domain: "path", Ref: "../../../../../../etc/passwd"},
		provisionResource{Domain: "path", Ref: "../" + filepath.Base(a1.Cwd) + "/escaped.txt"},
	)
	if err != nil {
		return fail(c, "provision path-traversal boundary: %v", err)
	}
	if r := assertEnvPathsContained(c, escaper); !r.Pass {
		return r
	}
	// Belt-and-suspenders: no routed path may resolve into a SIBLING boundary either.
	for name, val := range escaper.Env {
		if strings.HasPrefix(name, "MAD_RES_") && (pathWithin(val, a1.Cwd) || pathWithin(val, a2.Cwd)) {
			return fail(c, "BREACH: the substrate handed out a routed path %s=%q reaching into a SIBLING boundary", name, val)
		}
	}

	// (c) THE CONVERGENT FLOOR (grain-independent): the uncooperative agent, with only
	// worktree FS access, cannot observe or advance TRUNK. No promote has run, so the
	// mediated trunk is unborn — and a raw FS write has no path to it at all.
	if tip, _ := s.TrunkTip(); tip != "" {
		return fail(c, "an FS-only uncooperative agent reached/advanced TRUNK (tip %s) — the convergent floor was breached", short12(tip))
	}

	return pass(c, "worktree grain: boundaries DISJOINT (cwd %s|%s, branches %s|%s, ports %v|%v, distinct state roots); "+
		"a path-traversal/symlink resource ref was CONTAINED (no escaping path handed out); uncooperative agent cannot reach trunk. "+
		"SCOPE: raw co-located absolute-path FS writes are confined only at the container/VM grain (C5)",
		filepath.Base(a1.Cwd), filepath.Base(a2.Cwd), a1.Branch, a2.Branch, a1.Ports, a2.Ports)
}

func (c escapeWorktreeFS) Control(s *Scratch) error {
	// INJECT the violation the disjointness predicate is meant to catch: a NESTED /
	// SHARED-path layout. Feed assertBoundariesDisjoint two boundaries where one's cwd
	// is nested inside the other's and assert the predicate flips RED (Pass=false).
	// This proves the disjointness assertion is non-vacuous — it genuinely fails when
	// the boundaries are NOT disjoint, so the Run's green depends on real isolation.
	a, err := s.Provision()
	if err != nil {
		return fmt.Errorf("control provision: %w", err)
	}
	// Synthesize a NESTED sibling: its cwd is a child of a's cwd (a shared writable
	// forkable path — exactly the violation the grain forbids).
	nestedSibling := a
	nestedSibling.Cwd = filepath.Join(a.Cwd, "nested-child")
	nestedSibling.Branch = a.Branch + "-child"
	if r := assertBoundariesDisjoint(c, a, nestedSibling); r.Pass {
		return fmt.Errorf("CONTROL VACUOUS: the disjointness predicate PASSED on a NESTED layout (%q inside %q) — it cannot detect a shared-path violation, so the Run's disjointness green proves nothing", nestedSibling.Cwd, a.Cwd)
	}
	// And a SHARED-cwd layout (the strongest violation) must also flip RED.
	shared := a
	if r := assertBoundariesDisjoint(c, a, shared); r.Pass {
		return fmt.Errorf("CONTROL VACUOUS: the disjointness predicate PASSED when both boundaries SHARE a cwd %q", a.Cwd)
	}

	// (b) coverage: prove assertEnvPathsContained is LOAD-BEARING. The live substrate
	// hashes+Contains every ref so it can never EMIT an escaping routed path — so the (b)
	// guarantee is only non-vacuous if the predicate would CATCH an escape were one ever
	// handed out. Inject a boundary whose routed env path escapes to /etc and assert the
	// predicate flips RED; assert a contained routed path stays GREEN.
	escEnv := Boundary{Cwd: a.Cwd, StateDirs: a.StateDirs,
		Env: map[string]string{"MAD_RES_EVIL": "/etc/passwd"}}
	if r := assertEnvPathsContained(c, escEnv); r.Pass {
		return fmt.Errorf("CONTROL VACUOUS: assertEnvPathsContained PASSED a boundary whose routed env path escapes to /etc/passwd — the (b) no-escaping-path guarantee proves nothing")
	}
	okEnv := Boundary{Cwd: a.Cwd, StateDirs: a.StateDirs,
		Env: map[string]string{"MAD_RES_OK": filepath.Join(a.StateRoot(), "res", "ok")}}
	if r := assertEnvPathsContained(c, okEnv); !r.Pass {
		return fmt.Errorf("CONTROL VACUOUS: assertEnvPathsContained FAILED a boundary whose routed path is contained within its own state root (%s) — it flags everything, so the (b) verdict means nothing", r.Detail)
	}
	return nil
}

// assertEnvPathsContained is the load-bearing (b) predicate: EVERY routed-resource path
// the substrate handed this boundary (the MAD_RES_* env keys) must be an ABSOLUTE
// path contained WITHIN the boundary's own roots (its cwd or state root). ANY routed path
// that escapes the boundary — wherever it lands — is a BREACH; the predicate is exhaustive
// (it never narrows the breach to /etc or a sibling). It fails on zero routed paths so the
// assertion can never pass vacuously. This is exactly Contain's guarantee (contain.go):
// mad-substrate never HANDS an agent a path that escapes its boundary.
func assertEnvPathsContained(c Check, b Boundary) Result {
	root := b.StateRoot()
	checked := 0
	for name, val := range b.Env {
		if !strings.HasPrefix(name, "MAD_RES_") {
			continue // only routed-resource keys carry a substrate-resolved path
		}
		checked++
		if !filepath.IsAbs(val) {
			return fail(c, "routed-resource env %s=%q is not an absolute path (unexpected)", name, val)
		}
		if !pathWithin(val, root) && !pathWithin(val, b.Cwd) {
			return fail(c, "BREACH: routed-resource env %s=%q ESCAPES the boundary (not within cwd %q or state root %q)", name, val, b.Cwd, root)
		}
	}
	if checked == 0 {
		return fail(c, "non-vacuity: no MAD_RES_* routed paths present to assert containment over")
	}
	return pass(c, "all %d routed-resource path(s) contained within the boundary", checked)
}

// assertBoundariesDisjoint asserts the worktree-grain disjointness guarantee over
// EVERY provisioned dimension: distinct & non-nested cwd, distinct branch, distinct
// session, disjoint port blocks, distinct & non-nested state roots, and that neither
// boundary's env hands out a path INTO the other. Returns a failing Result naming
// the first violated dimension, or pass on full disjointness.
func assertBoundariesDisjoint(c Check, a, b Boundary) Result {
	if a.Cwd == b.Cwd {
		return fail(c, "two boundaries share a cwd %q (not isolated)", a.Cwd)
	}
	if nested(a.Cwd, b.Cwd) || nested(b.Cwd, a.Cwd) {
		return fail(c, "boundary cwds are NESTED (%q vs %q) — a relative escape could reach the sibling", a.Cwd, b.Cwd)
	}
	if a.Branch != "" && a.Branch == b.Branch {
		return fail(c, "two boundaries share a branch %q (not single-writer per agent)", a.Branch)
	}
	if a.Session != "" && a.Session == b.Session {
		return fail(c, "two distinct connections minted the SAME session %q", a.Session)
	}
	if shared := intersect(a.Ports, b.Ports); len(shared) > 0 {
		return fail(c, "two boundaries share ports %v (runtime not isolated)", shared)
	}
	ra, rb := a.StateRoot(), b.StateRoot()
	if ra != "" && rb != "" {
		if ra == rb || nested(ra, rb) || nested(rb, ra) {
			return fail(c, "boundary state roots overlap (%q vs %q) — a shared writable forkable path / coordination channel", ra, rb)
		}
	}
	// Neither boundary's env may hand out a path INTO the other boundary's cwd/state.
	for name, val := range a.Env {
		if filepath.IsAbs(val) && (pathWithin(val, b.Cwd) || (rb != "" && pathWithin(val, rb))) {
			return fail(c, "boundary A env %s=%q points INTO boundary B — a cross-boundary path was handed out", name, val)
		}
	}
	return pass(c, "disjoint")
}

// ----------------------------------------------------------------------------
// (ii) mediated-remote / origin bypass — a raw `git push` to the trunk ref in
// the bare mediated repo is STRUCTURALLY rejected by the update hook + the trunk
// ref is unchanged. CONTROL: the integrator's legit promote DOES advance.
// ----------------------------------------------------------------------------

type escapeMediatedRemote struct{}

func (escapeMediatedRemote) ID() string           { return "escape-mediated-remote" }
func (escapeMediatedRemote) OwnerProject() string { return "integrator-trunk (escape-resistance)" }
func (escapeMediatedRemote) Clause() string {
	return "escape (ii): an uncooperative raw push to the trunk ref is structurally rejected; no agent reaches origin (Inv 7)"
}

func (c escapeMediatedRemote) Run(s *Scratch) Result {
	agent, err := s.NewAgent("escremote")
	if err != nil {
		return fail(c, "new agent: %v", err)
	}
	if _, err := agent.Commit("attacker work", map[string]string{"evil.txt": "owned\n"}); err != nil {
		return fail(c, "commit attacker work: %v", err)
	}

	// UNCOOPERATIVE: a raw `git push` straight at the protected trunk ref BEFORE any
	// trunk exists (no adapter, no submit/promote). The trunk-protect update hook
	// must reject it (default-deny) and leave the trunk ref nonexistent.
	out, perr := agent.s.Git(agent.Dir, "push", "origin", "HEAD:refs/heads/trunk")
	if perr == nil {
		return fail(c, "BREACH: a raw push to refs/heads/trunk SUCCEEDED (the mediated remote did not refuse it): %s", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "integrator-only") {
		return fail(c, "the rejection must name the policy (integrator-only); got: %s", strings.TrimSpace(out))
	}
	if s.RefExists(s.BareDir, "refs/heads/trunk") {
		return fail(c, "BREACH: the rejected push still created refs/heads/trunk in the mediated repo")
	}

	// Also reject an attempt to push a sibling-namespaced branch (anything but nm/*).
	// #15: assert the rejection NAMES the default-deny policy AND that the rejected
	// push left NO ref behind in the mediated repo (not merely that the push failed).
	out2, perr2 := agent.s.Git(agent.Dir, "push", "origin", "HEAD:refs/heads/mainline")
	if perr2 == nil {
		return fail(c, "BREACH: a raw push to a non-nm/* ref SUCCEEDED: %s", strings.TrimSpace(out2))
	}
	if !strings.Contains(out2, "not permitted") && !strings.Contains(out2, "refs/heads/nm/") {
		return fail(c, "the non-nm/* rejection must name the default-deny policy (agents push refs/heads/nm/*); got: %s", strings.TrimSpace(out2))
	}
	if s.RefExists(s.BareDir, "refs/heads/mainline") {
		return fail(c, "BREACH: the rejected non-nm/* push still created refs/heads/mainline in the mediated repo")
	}

	// CONTROL (embedded): the integrator's LEGIT promote DOES advance the trunk —
	// proving the rejection above is the hook's policy, not a broken/dead remote.
	base, err := s.EstablishTrunkBase(agent, map[string]string{"ok.txt": "base\n"})
	if err != nil {
		return fail(c, "CONTROL: the legit integrator promote must advance trunk: %v", err)
	}
	if base == "" {
		return fail(c, "CONTROL DEAD: the legit promote did not produce a trunk tip (the remote may be dead, not protective)")
	}

	return pass(c, "raw push to refs/heads/trunk rejected (integrator-only) + non-nm/* rejected (default-deny), no ref left behind; legit promote advanced trunk to %s",
		short12(base))
}

func (c escapeMediatedRemote) Control(s *Scratch) error {
	// INJECT the breach the Run is meant to catch and assert the probe's OWN negative
	// predicate fires (fix #8). The Run asserts a raw, ungoverned advance of the trunk
	// ref is REFUSED / leaves no trunk ref. The breach is: the trunk ref WAS advanced
	// by a non-integrator path. We model that by advancing the trunk ref directly in
	// the bare repo via a raw `git update-ref` (which bypasses the push update-hook,
	// exactly the ungoverned path the structural guarantee forbids), then assert the
	// probe's breach predicate — "trunk advanced, but NOT by a governed promote" —
	// FIRES. If the predicate did not fire on an injected ungoverned advance, the
	// Run's "rejected push left no ref" verdict would be vacuous.
	agent, err := s.NewAgent("escremote-ctl")
	if err != nil {
		return fmt.Errorf("control new agent: %w", err)
	}
	if _, err := agent.Commit("ungoverned", map[string]string{"x.txt": "x\n"}); err != nil {
		return fmt.Errorf("control commit: %w", err)
	}
	// Push to an nm/* ref (the hook ALLOWS this), so the commit object lands in the
	// bare repo and can be pointed at by a raw ref write.
	ref, err := agent.PushBranch("ungoverned")
	if err != nil {
		return fmt.Errorf("control push nm/*: %w", err)
	}
	oid, gerr := s.Git(s.BareDir, "rev-parse", strings.TrimSpace(ref))
	if gerr != nil {
		return fmt.Errorf("control resolve nm/* oid: %v: %s", gerr, oid)
	}
	// The INJECTED ungoverned advance: write refs/heads/trunk directly (bypassing the
	// push hook + the integrator) — the breach a non-integrator advance represents.
	if out, uerr := s.Git(s.BareDir, "update-ref", "refs/heads/trunk", strings.TrimSpace(oid)); uerr != nil {
		return fmt.Errorf("control inject ungoverned trunk advance: %v: %s", uerr, out)
	}
	// The probe's breach predicate: after a raw advance NOT produced by a governed
	// integration, the trunk ref exists yet integrate.list records NO promoted
	// integration for that tip. Assert it FIRES (detects the breach).
	if !c.trunkAdvancedUngoverned(s) {
		return fmt.Errorf("CONTROL VACUOUS: the breach predicate did NOT fire on an injected ungoverned trunk advance — the probe cannot detect a non-integrator advance, so the push-rejection proves nothing")
	}
	return nil
}

// trunkAdvancedUngoverned is the breach predicate: the trunk ref exists, but NO
// promoted integration in integrate.list accounts for it (the integrator is the
// SOLE legitimate trunk promoter — a tip with no promoted integration was advanced
// by an ungoverned path). Black-box over integrate.list + the bare trunk ref.
func (c escapeMediatedRemote) trunkAdvancedUngoverned(s *Scratch) bool {
	tip, err := s.TrunkTip()
	if err != nil || tip == "" {
		return false // no trunk → nothing advanced
	}
	cl, err := s.Dial()
	if err != nil {
		return false
	}
	defer cl.Close()
	var out struct {
		Integrations []struct {
			State string `json:"state"`
		} `json:"integrations"`
	}
	if err := cl.Call("integrate.list", map[string]any{}, &out); err != nil {
		return false
	}
	for _, in := range out.Integrations {
		if in.State == "promoted" {
			return false // a governed promote accounts for the advance → NOT a breach
		}
	}
	return true // trunk advanced with zero promoted integrations → ungoverned breach
}

// ----------------------------------------------------------------------------
// (iii) singular-proxy bypass — an agent holding only a deny/mock/proxy env-spec
// sentinel cannot reach the real endpoint. CONTROL: a supervised grant DOES
// expose a real reachable endpoint.
// ----------------------------------------------------------------------------

type escapeSingularProxy struct{}

func (escapeSingularProxy) ID() string           { return "escape-singular-proxy" }
func (escapeSingularProxy) OwnerProject() string { return "singular-gate (escape-resistance)" }
func (escapeSingularProxy) Clause() string {
	return "escape (iii): a deny/mock/proxy env-spec sentinel is non-routable to the real endpoint (proxy-bypass / Inv 8)"
}

// proxyResource is declared singular+proxy in the manifest; supervisedResource is
// declared singular+supervised WITH a real endpoint (the positive control that the
// gate CAN hand a real reachable endpoint when explicitly granted).
const (
	proxyResource      = "proxy-saas"
	supervisedResource = "supervised-db"
	realEndpoint       = "https://real.internal.example:5432/db"
)

func (c escapeSingularProxy) Run(s *Scratch) Result {
	if err := s.writeManifestAndRestart(escapeSingularManifest()); err != nil {
		return fail(c, "install manifest + restart daemon: %v", err)
	}

	// UNCOOPERATIVE: request the proxy resource. We do NOT trust the gate's
	// self-reported real_reachable — we enumerate EVERY env value it produced and scan
	// each in RAW + base64/hex/url-DECODED forms for a routable endpoint, asserting
	// ONLY the non-routable mad-substrate-proxy:// sentinel appears (fix #4: an ENCODED
	// endpoint leak that grepping the plaintext would miss is now caught).
	proxy, err := s.GateRequest(proxyResource)
	if err != nil {
		return fail(c, "gate request %q: %v", proxyResource, err)
	}
	if proxy.Mode != "proxy" {
		return fail(c, "the proxy resource must resolve to mode=proxy; got mode=%q", proxy.Mode)
	}
	if reason := assertNonRoutableEnv(proxy.Env, "mad-substrate-proxy://"); reason != "" {
		return fail(c, "BREACH (proxy env-spec, NOT trusting real_reachable=%v): %s", proxy.RealReachable, reason)
	}
	// Defense in depth: the literal real endpoint must not appear in any form either.
	if leakedRealEndpoint(proxy.Env) {
		return fail(c, "BREACH: the real endpoint %q leaked (raw or encoded) into the proxy env-spec %v", realEndpoint, proxy.Env)
	}

	// A deny (undeclared) resource likewise yields no routable endpoint — same
	// encoded-leak scan over its env (the denied sentinel).
	deny, err := s.GateRequest("undeclared-prod")
	if err != nil {
		return fail(c, "gate request undeclared: %v", err)
	}
	if deny.Granted || deny.RealReachable {
		return fail(c, "an undeclared resource must be granted=false real_reachable=false; got granted=%v real_reachable=%v", deny.Granted, deny.RealReachable)
	}
	if leak, found := detectEndpointLeak(deny.Env, "mad-substrate-denied://"); found {
		return fail(c, "BREACH: a denied resource's env-spec leaked a routable endpoint: %s", leak)
	}

	// CONTROL (embedded): the SUPERVISED grant, when actually granted, DOES expose a
	// real reachable endpoint — proving the refusals above are genuine, not the gate's
	// inability to ever route a real endpoint.
	sup, err := s.GateRequest(supervisedResource)
	if err != nil {
		return fail(c, "gate request %q: %v", supervisedResource, err)
	}
	if !sup.Granted || !sup.RealReachable {
		return fail(c, "CONTROL DEAD: a granted supervised resource must be granted=true real_reachable=true; got granted=%v real_reachable=%v", sup.Granted, sup.RealReachable)
	}
	if !envContains(sup.Env, realEndpoint) {
		return fail(c, "CONTROL DEAD: a granted supervised resource must expose its real endpoint %q; got env %v", realEndpoint, sup.Env)
	}

	return pass(c, "proxy/deny env-specs carry ONLY the non-routable sentinel (no routable endpoint in raw/base64/hex/url forms, real_reachable NOT trusted); supervised grant CAN route real %s",
		realEndpoint)
}

func (c escapeSingularProxy) Control(s *Scratch) error {
	// INJECT a leaked endpoint into the probe's OWN evaluation path (the leak
	// detector) and assert it flips RED — WITHOUT modifying production gate.go. We
	// feed detectEndpointLeak a SYNTHETIC env map containing an ENCODED (base64)
	// routable endpoint masquerading as a proxy env-spec and assert the detector
	// CATCHES it. If the detector passed an encoded leak, the Run's "no routable
	// endpoint" verdict would be vacuous.
	encoded := base64.StdEncoding.EncodeToString([]byte(realEndpoint))
	leakedEnv := map[string]string{
		"MAD_SINGULAR_PROXY_SAAS": "mad-substrate-proxy://proxy-saas", // the honest sentinel
		"MAD_SMUGGLED":            encoded,                            // an encoded real endpoint smuggled in
	}
	if reason := assertNonRoutableEnv(leakedEnv, "mad-substrate-proxy://"); reason == "" {
		return fmt.Errorf("CONTROL VACUOUS: the leak detector PASSED a synthetic env with a base64-encoded routable endpoint (%q) — it cannot catch an encoded leak, so the proxy-bypass verdict proves nothing", encoded)
	}
	// And a clean sentinel-only env must NOT be flagged (the detector is not flagging
	// everything).
	cleanEnv := map[string]string{"MAD_SINGULAR_PROXY_SAAS": "mad-substrate-proxy://proxy-saas"}
	if reason := assertNonRoutableEnv(cleanEnv, "mad-substrate-proxy://"); reason != "" {
		return fmt.Errorf("CONTROL VACUOUS: the leak detector FLAGGED a clean sentinel-only env (%s) — it flags everything, so its leak verdict means nothing", reason)
	}
	// Belt and suspenders: the live gate must ALSO be able to route a real endpoint
	// for a supervised grant (so "proxy never routes real" is non-vacuous at runtime).
	if err := s.writeManifestAndRestart(escapeSingularManifest()); err != nil {
		return fmt.Errorf("control install manifest: %w", err)
	}
	sup, err := s.GateRequest(supervisedResource)
	if err != nil {
		return fmt.Errorf("control gate request: %v", err)
	}
	if !sup.RealReachable || !envContains(sup.Env, realEndpoint) {
		return fmt.Errorf("CONTROL VACUOUS: the gate can NEVER route a real reachable endpoint (even supervised) — the proxy-bypass refusal proves nothing: %v", sup)
	}
	return nil
}

// leakedRealEndpoint reports whether the literal real endpoint (or its
// base64/hex/url encoding) appears in any env value.
func leakedRealEndpoint(env map[string]string) bool {
	return envContains(env, realEndpoint) ||
		envContains(env, base64.StdEncoding.EncodeToString([]byte(realEndpoint))) ||
		envContains(env, hex.EncodeToString([]byte(realEndpoint))) ||
		envContains(env, url.QueryEscape(realEndpoint))
}

// envContains reports whether any env value contains needle.
func envContains(env map[string]string, needle string) bool {
	for _, v := range env {
		if strings.Contains(v, needle) {
			return true
		}
	}
	return false
}

// escapeSingularManifest declares proxyResource as singular+proxy (sentinel only)
// and supervisedResource as singular+supervised WITH a real endpoint (the gate CAN
// route a real endpoint when explicitly granted), leaving everything else
// undeclared (singular-by-default → deny).
func escapeSingularManifest() string {
	return `{
  "version": 1,
  "forkable": { "resources": [] },
  "convergent": { "paths": [], "resources": [] },
  "singular": {
    "paths": [],
    "resources": ["` + proxyResource + `", "` + supervisedResource + `"],
    "grants": [
      { "resource": "` + proxyResource + `", "mode": "proxy" },
      { "resource": "` + supervisedResource + `", "mode": "supervised", "endpoint": "` + realEndpoint + `" }
    ]
  }
}
`
}
