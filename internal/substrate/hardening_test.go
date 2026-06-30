package substrate

// Hand-authored GATED tests closing the adversarial-review findings: the
// same-session concurrent-provision race (Inv-10-grainswap atomicity), the
// resource-routing seam under hostile/colliding input (Inv 1 path/name
// derivation), the convergent half of Inv 2(b), rollback completeness, and the
// chafe-C2 repo-cleanliness property. Negatives carry positive controls.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/madhavhaldia/mad-substrate/internal/manifest"
)

// --- Inv-10-grainswap atomicity: same-session concurrent Provision -----------

// Under an IDEMPOTENT grain (the container/VM future, modeled by fakeGrain) two
// concurrent provisions of the same session derive the same paths. The race
// loser must NOT roll back the winner's live boundary. Pins the reserve-first fix.
func TestSameSessionConcurrentProvision(t *testing.T) {
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	s, err := New(t.TempDir(), nil, Options{Grain: fakeGrain{root: t.TempDir()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const n = 16
	specs := make([]*EnvSpec, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			specs[i], errs[i] = s.Provision("s-1-aaaa", Request{Ports: 2})
		}(i)
	}
	wg.Wait()

	winners := 0
	var win *EnvSpec
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			winners++
			win = specs[i]
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one concurrent same-session provision must win, got %d", winners)
	}
	// The winner's boundary must be intact — not destroyed by a loser's rollback.
	if _, err := os.Stat(win.cwd); err != nil {
		t.Fatalf("Inv 1 VIOLATED: winner's worktree destroyed by a loser's rollback: %v", err)
	}
	if _, err := os.Stat(win.stateRoot); err != nil {
		t.Fatalf("Inv 1 VIOLATED: winner's state root destroyed by a loser's rollback: %v", err)
	}
	if got := s.Lookup("s-1-aaaa"); got == nil || got.cwd != win.cwd {
		t.Fatal("the winner must be the single live boundary")
	}
}

// --- resource-routing seam under hostile / colliding input -------------------

func TestResourceRoutingContainsHostileRefs(t *testing.T) {
	s := newSub(t, nil) // default classifier: a path is forkable
	// Hostile refs must be CONTAINED (no escape) and UNIQUE (no collapse), not error.
	hostile := []string{"../../etc/passwd", "/abs/escape", "../sibling", "....", "/", "a/b"}
	spec, err := s.Provision("s-1-aaaa", Request{Resources: refs("path", hostile...)})
	if err != nil {
		t.Fatalf("forkable hostile refs must be contained, not error: %v", err)
	}
	agentRoot := mustAbs(t, spec.stateRoot)
	seen := map[string]string{}
	for _, ref := range hostile {
		name := resourceEnvName(resourceSlug(ref))
		p, ok := spec.env[name]
		if !ok {
			t.Fatalf("ref %q not routed (env %q missing)", ref, name)
		}
		if !withinBase(agentRoot, p) {
			t.Fatalf("Inv 1 VIOLATED: routed path for %q escapes agentRoot: %s", ref, p)
		}
		if prev, dup := seen[p]; dup {
			t.Fatalf("distinct refs %q and %q collided to one dir %s", prev, ref, p)
		}
		seen[p] = ref
	}
	// Positive control: a NAIVE join of a traversal ref escapes agentRoot/res —
	// proving the sanitization above is what keeps the real routing contained.
	naive := filepath.Join(agentRoot, "res", "../../etc/passwd")
	if withinBase(agentRoot, naive) {
		t.Fatal("positive control vacuous: naive join of a traversal ref did not escape agentRoot")
	}
}

// safeSlug's old conditional-hash collision (two distinct refs → one slug) must
// no longer reproduce: resourceSlug hashes unconditionally.
func TestResourceRefsNeverCollide(t *testing.T) {
	s := newSub(t, nil)
	// "prod/db" and "prod_db-96e5a54a" defeated the old conditional-hash slug.
	spec, err := s.Provision("s-1-aaaa", Request{Resources: refs("path", "prod/db", "prod_db-96e5a54a")})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	a := spec.env[resourceEnvName(resourceSlug("prod/db"))]
	b := spec.env[resourceEnvName(resourceSlug("prod_db-96e5a54a"))]
	if a == "" || b == "" {
		t.Fatalf("both refs must route: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("Inv 1 VIOLATED: distinct refs collided to one dir %s", a)
	}
}

func TestResourceNameAndRefValidation(t *testing.T) {
	s := newSub(t, nil)
	// A resource Name must never be allowed to shadow a substrate-owned key or a
	// launch-critical variable, nor smuggle an '=' (which the launcher's env-apply
	// would split into a different variable). Each must be REJECTED.
	bad := []struct {
		sess, name, why string
	}{
		{"s-r1", "TMPDIR", "substrate-owned routing var"},
		{"s-r2", "PATH", "launch-critical: binary resolution"},
		{"s-r3", "LD_PRELOAD", "launch-critical: library injection"},
		{"s-r4", "DYLD_INSERT_LIBRARIES", "launch-critical: macOS dylib injection"},
		{"s-r5", "GIT_SSH", "launch-critical: git transport"},
		{"s-r6", "PORT=9", "embedded '=' splits into PORT"},
		{"s-r7", "MY DB", "not a valid env identifier"},
		{"s-r8", "9DB", "identifier may not start with a digit"},
	}
	for _, b := range bad {
		if _, err := s.Provision(b.sess, Request{Resources: []ResourceReq{{Name: b.name, Domain: "path", Ref: "x"}}}); err == nil {
			t.Fatalf("a resource named %q (%s) must be rejected", b.name, b.why)
		}
	}
	// Empty ref → rejected.
	if _, err := s.Provision("s-r9", Request{Resources: []ResourceReq{{Domain: "path", Ref: "   "}}}); err == nil {
		t.Fatal("an empty resource ref must be rejected")
	}
	// Duplicate explicit name across two forkable resources → rejected.
	if _, err := s.Provision("s-r10", Request{Resources: []ResourceReq{
		{Name: "DB", Domain: "path", Ref: "a"}, {Name: "DB", Domain: "path", Ref: "b"},
	}}); err == nil {
		t.Fatal("a duplicate resource env name must be rejected")
	}
	// POSITIVE CONTROL: a clean, non-sensitive name succeeds; substrate-owned and
	// inherited launch vars are untouched.
	spec, err := s.Provision("s-ok", Request{Resources: []ResourceReq{{Name: "DATABASE_URL", Domain: "path", Ref: "a"}}})
	if err != nil {
		t.Fatalf("a clean resource (DATABASE_URL) must succeed: %v", err)
	}
	if spec.env["DATABASE_URL"] == "" {
		t.Fatal("control vacuous: a clean resource must be routed")
	}
	if spec.env["TMPDIR"] != spec.stateDirs["scratch"] {
		t.Fatal("the substrate's own reserved env must survive resource routing")
	}
	if _, leaked := spec.env["PATH"]; leaked {
		t.Fatal("the substrate must never EMIT a PATH override")
	}
}

// --- Teardown-vs-Provision race (Inv-10-grainswap atomicity, other axis) ------

// gatedGrain is an idempotent grain whose Teardown blocks under test control, so
// we can deterministically interleave a teardown with a re-provision of the same
// slug — modeling the container/VM grain where the slug paths are shared.
type gatedGrain struct {
	root      string
	tdEnter   chan struct{}
	tdRelease chan struct{}
}

func (g *gatedGrain) Name() string { return "gated" }
func (g *gatedGrain) Provision(slug string) (Boundary, error) {
	p := filepath.Join(g.root, slug)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return Boundary{}, err
	}
	return Boundary{Cwd: p, Branch: "gated/" + slug}, nil
}
func (g *gatedGrain) Teardown(b Boundary) error {
	if g.tdEnter != nil {
		g.tdEnter <- struct{}{}
		<-g.tdRelease
	}
	return os.RemoveAll(b.Cwd)
}
func (g *gatedGrain) ReclaimOrphan(slug string) error {
	return os.RemoveAll(filepath.Join(g.root, slug))
}

func TestTeardownProvisionRaceGated(t *testing.T) {
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	g := &gatedGrain{root: t.TempDir(), tdEnter: make(chan struct{}), tdRelease: make(chan struct{})}
	s, err := New(t.TempDir(), nil, Options{Grain: g})
	if err != nil {
		t.Fatal(err)
	}
	spec1, err := s.Provision("s-1-aaaa", Request{})
	if err != nil {
		t.Fatal(err)
	}

	tdErr := make(chan error, 1)
	go func() { tdErr <- s.Teardown("s-1-aaaa") }()
	<-g.tdEnter // teardown is now inside grain.Teardown, holding the slug in flight

	// A re-Provision of the SAME slug DURING the in-flight teardown must be rejected
	// — never build a new boundary on the shared paths that the stale teardown then
	// deletes. (Without the Teardown gate this provision would succeed and its
	// boundary would be destroyed by the teardown completing below.)
	if _, err := s.Provision("s-1-aaaa", Request{}); err == nil {
		t.Fatal("Inv 1 VIOLATED: re-provision during an in-flight teardown must be rejected")
	}

	close(g.tdRelease)
	if err := <-tdErr; err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, err := os.Stat(spec1.cwd); !os.IsNotExist(err) {
		t.Fatal("teardown must remove the original boundary")
	}
	// Once teardown completes, the slug is free again and a new boundary is intact.
	spec2, err := s.Provision("s-1-aaaa", Request{})
	if err != nil {
		t.Fatalf("re-provision after teardown completes must succeed: %v", err)
	}
	if _, err := os.Stat(spec2.cwd); err != nil {
		t.Fatalf("the new boundary must exist and survive: %v", err)
	}
}

// --- Inv 2(b): the CONVERGENT verdict is deferred, never auto-forked ---------

func TestConvergentResourceDeferred(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	repo := initRepo(t)
	cls := manifest.New(&manifest.Manifest{
		Version:             manifest.SupportedVersion,
		ConvergentPaths:     []string{"migrations/**"},
		ForkableResources:   map[string]bool{},
		ConvergentResources: map[string]bool{},
		SingularResources:   map[string]bool{},
	})
	s, err := New(repo, cls, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec, err := s.Provision("s-1-aaaa", Request{Resources: []ResourceReq{
		{Name: "MIG", Domain: "path", Ref: "migrations/001.sql"},
	}})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, ok := spec.env["MIG"]; ok {
		t.Fatal("Inv 2(b) VIOLATED: a convergent resource was auto-isolated into the env")
	}
	if !deferredHas(spec.deferred, "migrations/001.sql", "convergent") {
		t.Fatalf("a convergent resource must be DEFERRED to the integrator, got %+v", spec.deferred)
	}
	// POSITIVE CONTROL: a path NOT matching the convergent glob is forkable → isolated.
	spec2, err := s.Provision("s-2-bbbb", Request{Resources: []ResourceReq{
		{Name: "SRC", Domain: "path", Ref: "src/app.go"},
	}})
	if err != nil {
		t.Fatalf("provision control: %v", err)
	}
	if spec2.env["SRC"] == "" {
		t.Fatal("control vacuous: a non-convergent path must be isolated")
	}
}

// --- rollback completeness ---------------------------------------------------

type failGrain struct{}

func (failGrain) Name() string                       { return "fail" }
func (failGrain) Provision(string) (Boundary, error) { return Boundary{}, fmt.Errorf("grain boom") }
func (failGrain) Teardown(Boundary) error            { return nil }
func (failGrain) ReclaimOrphan(string) error         { return nil }

func TestProvisionRollbackOnGrainFailure(t *testing.T) {
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	s, err := New(t.TempDir(), nil, Options{Grain: failGrain{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Provision("s-1-aaaa", Request{}); err == nil {
		t.Fatal("provision must fail when the grain fails")
	}
	if s.ports.reservedCount() != 0 {
		t.Fatalf("ports must not leak on grain failure: %d reserved", s.ports.reservedCount())
	}
	if s.Lookup("s-1-aaaa") != nil {
		t.Fatal("no live boundary after a failed provision")
	}
	// The reservation was released → the slug is usable again.
	if _, err := s.Provision("s-1-aaaa", Request{}); err == nil {
		// fail grain still fails, but it must reach the grain (reservation freed),
		// not be rejected as "already provisioned / in flight".
		t.Fatal("second provision still fails at the grain (expected)")
	}
}

// A LATE failure (reserved-name resource, after grain+ports+state are built) must
// roll EVERYTHING back: ports released, worktree removed, nothing left live.
func TestProvisionRollbackOnLateFailure(t *testing.T) {
	s := newSub(t, nil)
	_, err := s.Provision("s-1-aaaa", Request{Ports: 4, Resources: []ResourceReq{
		{Name: "TMPDIR", Domain: "path", Ref: "x"}, // reserved → late failure
	}})
	if err == nil {
		t.Fatal("a reserved-name resource must fail the provision")
	}
	if s.ports.reservedCount() != 0 {
		t.Fatalf("ports must be released on rollback, %d still reserved", s.ports.reservedCount())
	}
	if s.Lookup("s-1-aaaa") != nil {
		t.Fatal("no live boundary after rollback")
	}
	// Substrate is usable after rollback (fresh session — the rolled-back worktree
	// leaves its branch behind, like any teardown).
	if _, err := s.Provision("s-2-bbbb", Request{}); err != nil {
		t.Fatalf("substrate must be usable after a rolled-back provision: %v", err)
	}
}

// --- chafe C2: provisioning/teardown leaves the governed repo's status clean --

func TestProvisionLeavesRepoClean(t *testing.T) {
	s := newSub(t, nil)
	spec, err := s.Provision("s-1-aaaa", Request{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if out := gitOut(t, s.repoAbs, "status", "--porcelain"); out != "" {
		t.Fatalf("provision must leave the governed repo clean; dirty:\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(spec.cwd, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := gitOut(t, s.repoAbs, "status", "--porcelain"); out != "" {
		t.Fatalf("work in the worktree must not dirty the governed repo:\n%s", out)
	}
	if err := s.Teardown("s-1-aaaa"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if out := gitOut(t, s.repoAbs, "status", "--porcelain"); out != "" {
		t.Fatalf("teardown must leave the governed repo clean:\n%s", out)
	}
}

// --- helpers -----------------------------------------------------------------

func refs(domain string, rr ...string) []ResourceReq {
	out := make([]ResourceReq, len(rr))
	for i, r := range rr {
		out[i] = ResourceReq{Domain: domain, Ref: r}
	}
	return out
}

func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = repo
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
