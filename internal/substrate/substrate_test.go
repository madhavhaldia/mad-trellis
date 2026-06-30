package substrate

// Hand-authored GATED invariant tests for project 4 (isolation-substrate):
// Inv 1 (forkable FS + ports + local-state isolation), Inv 2(b) (what to isolate
// is the classifier's verdict, not the substrate's), Inv 10-grainswap (the dial).
// Negative/absence assertions carry positive controls.

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/madhavhaldia/mad-substrate/internal/manifest"
)

// --- helpers ----------------------------------------------------------------

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "t@t")
	gitRun(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// newSub builds a Substrate over a fresh git repo with isolated worktree/state
// roots (nothing touches $HOME). cls nil → default classifier.
func newSub(t *testing.T, cls *manifest.Classifier) *Substrate {
	t.Helper()
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	repo := initRepo(t)
	s, err := New(repo, cls, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// fileVisibleAcross reports whether a file written under aPath is observable
// under bPath — i.e. the two are NOT isolated. The cross-visibility predicate.
func fileVisibleAcross(t *testing.T, aPath, bPath string) bool {
	t.Helper()
	marker := filepath.Join(aPath, "iso-marker.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(bPath, "iso-marker.txt"))
	return err == nil
}

func intersects(a, b []int) bool {
	set := map[int]struct{}{}
	for _, p := range a {
		set[p] = struct{}{}
	}
	for _, p := range b {
		if _, ok := set[p]; ok {
			return true
		}
	}
	return false
}

// --- Inv 1: forkable FS isolation (cross-visibility) ------------------------

func TestForkableFSIsolation(t *testing.T) {
	s := newSub(t, nil)
	a, err := s.Provision("s-1-aaaa", Request{})
	if err != nil {
		t.Fatalf("provision A: %v", err)
	}
	b, err := s.Provision("s-2-bbbb", Request{})
	if err != nil {
		t.Fatalf("provision B: %v", err)
	}

	if a.cwd == b.cwd || a.branch == b.branch {
		t.Fatalf("worktrees must be distinct: %q/%q  %q/%q", a.cwd, b.cwd, a.branch, b.branch)
	}
	if !disjoint(a.cwd, b.cwd) {
		t.Fatalf("worktree roots must be non-nested: %q vs %q", a.cwd, b.cwd)
	}
	// A's file is invisible from B.
	if fileVisibleAcross(t, a.cwd, b.cwd) {
		t.Fatal("Inv 1 VIOLATED: a file written in A's worktree is visible in B's")
	}

	// POSITIVE CONTROL: point both at the SAME dir and prove the cross-visibility
	// predicate detects sharing (so the assertion above is non-vacuous).
	shared := t.TempDir()
	if !fileVisibleAcross(t, shared, shared) {
		t.Fatal("positive control vacuous: cross-visibility predicate failed to detect a shared writable path")
	}
}

// --- Inv 1: per-agent PORT disjointness -------------------------------------

func TestPortDisjointness(t *testing.T) {
	s := newSub(t, nil)
	a, err := s.Provision("s-1-aaaa", Request{Ports: 4})
	if err != nil {
		t.Fatalf("provision A: %v", err)
	}
	b, err := s.Provision("s-2-bbbb", Request{Ports: 4})
	if err != nil {
		t.Fatalf("provision B: %v", err)
	}
	if len(a.ports) != 4 || len(b.ports) != 4 {
		t.Fatalf("each agent must get its requested port block: %v %v", a.ports, b.ports)
	}
	if intersects(a.ports, b.ports) {
		t.Fatalf("Inv 1 VIOLATED: port sets overlap: %v ∩ %v", a.ports, b.ports)
	}
}

func TestConcurrentProvisionDisjointPorts(t *testing.T) {
	s := newSub(t, nil)
	const n = 8
	specs := make([]*EnvSpec, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			specs[i], errs[i] = s.Provision(sessionID(i), Request{Ports: 3})
		}(i)
	}
	wg.Wait()
	seen := map[int]int{}
	for i, sp := range specs {
		if errs[i] != nil {
			t.Fatalf("provision %d: %v", i, errs[i])
		}
		for _, p := range sp.ports {
			if prev, dup := seen[p]; dup {
				t.Fatalf("Inv 1 VIOLATED: port %d handed to sessions %d and %d", p, prev, i)
			}
			seen[p] = i
		}
	}
	// Worktree/git-admin integrity under concurrency (the grain-serialization
	// guarantee): all worktrees distinct & non-nested, not just disjoint ports.
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if !disjoint(specs[i].cwd, specs[j].cwd) {
				t.Fatalf("worktrees %d and %d not disjoint: %q / %q", i, j, specs[i].cwd, specs[j].cwd)
			}
		}
	}
}

// --- Inv 1: per-agent local-state disjointness ------------------------------

func TestLocalStateDisjointness(t *testing.T) {
	s := newSub(t, nil)
	a, err := s.Provision("s-1-aaaa", Request{})
	if err != nil {
		t.Fatalf("provision A: %v", err)
	}
	b, err := s.Provision("s-2-bbbb", Request{})
	if err != nil {
		t.Fatalf("provision B: %v", err)
	}
	if !disjoint(a.stateRoot, b.stateRoot) {
		t.Fatalf("state roots must be non-nested: %q vs %q", a.stateRoot, b.stateRoot)
	}
	for _, role := range stateRoles {
		ap, bp := a.stateDirs[role], b.stateDirs[role]
		if ap == "" || bp == "" {
			t.Fatalf("missing %s dir: %q/%q", role, ap, bp)
		}
		if ap == bp {
			t.Fatalf("Inv 1 VIOLATED: shared %s dir: %q", role, ap)
		}
		if fileVisibleAcross(t, ap, bp) {
			t.Fatalf("Inv 1 VIOLATED: %s state visible across agents", role)
		}
	}
	// The env routes the common toolchain vars at the per-agent dirs.
	if a.env["TMPDIR"] != a.stateDirs["scratch"] || a.env["XDG_CACHE_HOME"] != a.stateDirs["cache"] {
		t.Fatal("env must route TMPDIR/XDG_CACHE_HOME at the per-agent dirs")
	}
}

// --- Inv 2(b): what-to-isolate is the classifier's verdict ------------------

func TestIsolationIsClassifierGated(t *testing.T) {
	// Default classifier: a working-tree PATH is forkable; a silent EXTERNAL
	// resource is singular (default-deny). The substrate must isolate ONLY the
	// forkable one and DEFER the singular one — never auto-fork it.
	s := newSub(t, nil)
	spec, err := s.Provision("s-1-aaaa", Request{Resources: []ResourceReq{
		{Name: "FORK_DIR", Domain: "path", Ref: "src/component"},
		{Name: "PROD_DB", Domain: "external", Ref: "prod-db"},
	}})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, ok := spec.env["FORK_DIR"]; !ok {
		t.Fatal("a forkable resource must be isolated into the env-spec")
	}
	if _, ok := spec.env["PROD_DB"]; ok {
		t.Fatal("Inv 2(b)/9 VIOLATED: a silent external (singular) resource was auto-isolated as forkable")
	}
	if !deferredHas(spec.deferred, "prod-db", "singular") {
		t.Fatalf("the singular resource must be DEFERRED (routed to the gate), got %+v", spec.deferred)
	}

	// POSITIVE CONTROL: flip the classifier verdict by DECLARING prod-db forkable.
	// The same resource must now be isolated — proving the routing tracks the
	// classifier, not a hardcoded substrate policy (non-vacuous).
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	repo := initRepo(t)
	cls := manifest.New(&manifest.Manifest{
		Version:             manifest.SupportedVersion,
		ForkableResources:   map[string]bool{"prod-db": true},
		ConvergentResources: map[string]bool{},
		SingularResources:   map[string]bool{},
	})
	s2, err := New(repo, cls, Options{})
	if err != nil {
		t.Fatalf("New control: %v", err)
	}
	spec2, err := s2.Provision("s-1-aaaa", Request{Resources: []ResourceReq{
		{Name: "PROD_DB", Domain: "external", Ref: "prod-db"},
	}})
	if err != nil {
		t.Fatalf("provision control: %v", err)
	}
	if _, ok := spec2.env["PROD_DB"]; !ok {
		t.Fatal("control vacuous: a DECLARED-forkable resource was not isolated — routing ignores the verdict")
	}
	if deferredHas(spec2.deferred, "prod-db", "singular") {
		t.Fatal("a declared-forkable resource must NOT be deferred as singular")
	}
}

// Determinism / no-inference: the isolate decision is a pure predicate over the
// classifier verdict — identical inputs yield identical routing every call.
func TestRoutingIsDeterministic(t *testing.T) {
	cls := manifest.New(nil)
	ref := manifest.ExternalRef("prod-db")
	first := shouldIsolate(cls.Classify(ref))
	for i := 0; i < 1000; i++ {
		if shouldIsolate(cls.Classify(ref)) != first {
			t.Fatal("Inv 2(b) VIOLATED: isolate decision is non-deterministic")
		}
	}
	if first {
		t.Fatal("a silent external resource must not be isolated (classify-upward)")
	}
}

// --- Inv 10-grainswap: the dial ---------------------------------------------

type fakeGrain struct{ root string }

func (g fakeGrain) Name() string { return "fake" }
func (g fakeGrain) Provision(slug string) (Boundary, error) {
	p := filepath.Join(g.root, slug)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return Boundary{}, err
	}
	return Boundary{Cwd: p, Branch: "fake/" + slug}, nil
}
func (g fakeGrain) Teardown(b Boundary) error { return os.RemoveAll(b.Cwd) }
func (g fakeGrain) ReclaimOrphan(slug string) error {
	return os.RemoveAll(filepath.Join(g.root, slug))
}

func TestGrainSwapParity(t *testing.T) {
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	// Swap the FS grain entirely (no git). The SAME Substrate.Provision code must
	// yield a contract-valid env-spec with the caller unchanged (10-grainswap).
	s, err := New(t.TempDir(), nil, Options{Grain: fakeGrain{root: t.TempDir()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec, err := s.Provision("s-1-aaaa", Request{})
	if err != nil {
		t.Fatalf("provision (fake grain): %v", err)
	}
	if spec.grain != "fake" {
		t.Fatalf("grain name must reflect the dial: %q", spec.grain)
	}
	if spec.cwd == "" || len(spec.ports) == 0 || spec.stateDirs["scratch"] == "" {
		t.Fatalf("env-spec must be contract-valid regardless of grain: %+v", spec.Wire())
	}
}

// --- teardown round-trip (mechanism) ----------------------------------------

func TestTeardownRoundTrip(t *testing.T) {
	s := newSub(t, nil)
	spec, err := s.Provision("s-1-aaaa", Request{Ports: 4})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if s.ports.reservedCount() != 4 {
		t.Fatalf("expected 4 reserved ports, got %d", s.ports.reservedCount())
	}
	if _, err := os.Stat(spec.cwd); err != nil {
		t.Fatalf("worktree must exist after provision: %v", err)
	}

	if err := s.Teardown("s-1-aaaa"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, err := os.Stat(spec.cwd); !os.IsNotExist(err) {
		t.Fatal("worktree must be gone after teardown")
	}
	if _, err := os.Stat(spec.stateRoot); !os.IsNotExist(err) {
		t.Fatal("local-state root must be gone after teardown")
	}
	if s.ports.reservedCount() != 0 {
		t.Fatalf("ports must be released after teardown, %d still reserved", s.ports.reservedCount())
	}
	if s.Lookup("s-1-aaaa") != nil {
		t.Fatal("session must not be live after teardown")
	}
	// Teardown frees the RUNTIME boundary but PRESERVES the branch — the agent's
	// commits may not be integrated yet; deleting it would lose work. The
	// integrator (P4) consumes/retires the branch, not teardown.
	if !branchExists(t, s.repoAbs, spec.branch) {
		t.Fatalf("teardown must preserve the work branch %q for the integrator", spec.branch)
	}
	// Idempotent: a second teardown is a no-op.
	if err := s.Teardown("s-1-aaaa"); err != nil {
		t.Fatalf("teardown must be idempotent: %v", err)
	}
	// The substrate is usable after teardown. (Real sessions are daemon-minted and
	// unique — they never repeat — so a fresh id is the realistic re-provision.)
	if _, err := s.Provision("s-9-zzzz", Request{}); err != nil {
		t.Fatalf("provision after teardown must succeed: %v", err)
	}
}

func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	c := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	c.Dir = repo
	return c.Run() == nil
}

// Double provision of a live session is refused (one boundary per session).
func TestDoubleProvisionRefused(t *testing.T) {
	s := newSub(t, nil)
	if _, err := s.Provision("s-1-aaaa", Request{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := s.Provision("s-1-aaaa", Request{}); err == nil {
		t.Fatal("provisioning an already-live session must be refused")
	}
}

// EnvSpec is immutable: accessors hand back copies, so a consumer cannot mutate
// the live boundary.
func TestEnvSpecImmutable(t *testing.T) {
	s := newSub(t, nil)
	spec, err := s.Provision("s-1-aaaa", Request{Ports: 2})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := spec.Env()
	e["INJECTED"] = "evil"
	if _, leaked := spec.Env()["INJECTED"]; leaked {
		t.Fatal("EnvSpec.Env() must return a copy — the live boundary leaked a mutation")
	}
	p := spec.Ports()
	if len(p) > 0 {
		p[0] = -1
		if spec.Ports()[0] == -1 {
			t.Fatal("EnvSpec.Ports() must return a copy")
		}
	}
}

func deferredHas(ds []DeferredResource, name, kind string) bool {
	for _, d := range ds {
		if d.Name == name && d.Kind == kind {
			return true
		}
	}
	return false
}

func sessionID(i int) string {
	return "s-" + string(rune('a'+i)) + "-conc"
}
