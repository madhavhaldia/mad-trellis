package manifest

// Hand-authored invariant tests for project 3 (docs/0004 card 3) — the contract.
// Review-gated; the classify-upward total function and declaration-not-
// modification proofs are the soul. NOT vibe-coded.

import (
	"bytes"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func testManifest() *Manifest {
	m := DefaultManifest()
	m.ConvergentPaths = []string{"migrations/**", "**/*.lock"}
	m.SingularPaths = []string{"secrets/**"}
	m.ForkableResources = map[string]bool{"postgres-local": true}
	m.ConvergentResources = map[string]bool{"shared-cache": true}
	m.SingularResources = map[string]bool{"prod-db": true}
	// "ambi" is declared both forkable and singular → conflicting → strictest.
	m.ForkableResources["ambi"] = true
	m.SingularResources["ambi"] = true
	return m
}

// --- Inv 9: classify-upward, TOTAL ------------------------------------------

func TestClassifyKnownCases(t *testing.T) {
	c := New(testManifest())
	cases := []struct {
		ref  ResourceRef
		want Kind
	}{
		{TrunkRef(), Convergent},                               // the trunk is always convergent
		{PathRef("src/auth.go"), Forkable},                     // silent path → forkable (worktree copy)
		{PathRef("migrations/0001_init.sql"), Convergent},      // declared convergent path
		{PathRef("pkg/go.lock"), Convergent},                   // **/*.lock
		{PathRef("secrets/key.pem"), Singular},                 // declared singular path
		{ExternalRef("postgres-local"), Forkable},              // declared forkable resource
		{ExternalRef("shared-cache"), Convergent},              // declared convergent resource
		{ExternalRef("prod-db"), Singular},                     // declared singular resource
		{ExternalRef("undeclared-saas"), Singular},             // SILENT external → default-deny
		{ExternalRef("ambi"), Singular},                        // conflicting decls → strictest
		{ResourceRef{Domain: Domain(99), Name: "x"}, Singular}, // unknown domain → strictest
	}
	for _, tc := range cases {
		got := c.Classify(tc.ref)
		if got != tc.want {
			t.Errorf("Classify(%+v) = %v, want %v", tc.ref, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("Classify(%+v) returned an invalid kind %d", tc.ref, got)
		}
	}
}

func TestClassifyIsTotal(t *testing.T) {
	c := New(testManifest())
	r := rand.New(rand.NewSource(1)) // fixed seed: deterministic
	for i := 0; i < 5000; i++ {
		dom := Domain(r.Intn(5)) // 0..4 includes invalid domains 3,4
		name := randName(r)
		k := c.Classify(ResourceRef{Domain: dom, Name: name})
		if !k.Valid() {
			t.Fatalf("Classify must be TOTAL (a valid kind) for domain=%d name=%q; got %d", dom, name, k)
		}
		// Positive control on classify-upward: an undeclared external is ALWAYS
		// singular — never looser. If this ever returns forkable/convergent, a
		// downward hole exists.
		if dom == DomainExternal &&
			!c.m.ForkableResources[name] && !c.m.ConvergentResources[name] && !c.m.SingularResources[name] {
			if k != Singular {
				t.Fatalf("undeclared external %q must be singular; got %v", name, k)
			}
		}
	}
}

func TestDeterminism(t *testing.T) {
	c := New(testManifest())
	refs := []ResourceRef{TrunkRef(), PathRef("a/b.go"), PathRef("migrations/x.sql"), ExternalRef("prod-db"), ExternalRef("zzz")}
	for _, ref := range refs {
		k0 := c.Classify(ref)
		_, key0 := c.Route(ref)
		for i := 0; i < 100; i++ {
			if k := c.Classify(ref); k != k0 {
				t.Fatalf("Classify(%+v) non-deterministic: %v vs %v", ref, k, k0)
			}
			if _, key := c.Route(ref); !bytes.Equal(key, key0) {
				t.Fatalf("Route(%+v) key non-deterministic", ref)
			}
		}
	}
}

// --- domain-aware convergent routing (per-path keys) ------------------------

func TestTrunkRoutesToTrunkKey(t *testing.T) {
	c := New(testManifest())
	// The trunk-merge lease (conductor/integrator serialize on it) is UNCHANGED.
	k, key := c.Route(TrunkRef())
	if k != Convergent {
		t.Fatalf("trunk must be convergent; got %v", k)
	}
	if !bytes.Equal(key, TrunkKey) {
		t.Fatalf("trunk domain must route to TrunkKey; got %q", key)
	}
	// A convergent EXTERNAL must NOT collapse onto TrunkKey (R11) — it gets its
	// own per-name key so distinct external resources no longer falsely serialize.
	if _, ekey := c.Route(ExternalRef("shared-cache")); bytes.Equal(ekey, TrunkKey) {
		t.Fatalf("convergent external must NOT collapse onto TrunkKey (R11); got %q", ekey)
	}
}

func TestConvergentExternalGetsDistinctPerNameKeys(t *testing.T) {
	m := testManifest()
	// Two DISTINCT convergent EXTERNAL resources (R11): they must get DISTINCT
	// per-name keys, neither == TrunkKey, so two agents on different convergent
	// external resources no longer falsely serialize.
	m.ConvergentResources = map[string]bool{"shared-cache": true, "metrics-sink": true}
	c := New(m)

	k1, k1key := c.Route(ExternalRef("shared-cache"))
	k2, k2key := c.Route(ExternalRef("metrics-sink"))
	if k1 != Convergent || k2 != Convergent {
		t.Fatalf("both must be convergent; got %v %v", k1, k2)
	}
	if len(k1key) == 0 || len(k2key) == 0 {
		t.Fatalf("convergent externals must carry a lease key; got %q %q", k1key, k2key)
	}
	// CONTROL: distinct names → distinct keys (proves they no longer collapse).
	if bytes.Equal(k1key, k2key) {
		t.Fatalf("distinct convergent externals must get distinct keys; both = %q", k1key)
	}
	// CONTROL: neither external key is the global trunk key (trunk untouched).
	if bytes.Equal(k1key, TrunkKey) || bytes.Equal(k2key, TrunkKey) {
		t.Fatalf("convergent external must NOT collapse onto TrunkKey (R11); k1=%q k2=%q", k1key, k2key)
	}
	// CONTROL: DomainTrunk still returns the single global trunk key (untouched).
	if _, tkey := c.Route(TrunkRef()); !bytes.Equal(tkey, TrunkKey) {
		t.Fatalf("trunk domain must still route to the global TrunkKey; got %q", tkey)
	}

	// Round-trip: ExternalFromConvergentKey recovers the resource name for display.
	if n, ok := ExternalFromConvergentKey(k1key); !ok || n != "shared-cache" {
		t.Fatalf("ExternalFromConvergentKey(k1key) = (%q,%v), want (shared-cache,true)", n, ok)
	}
	if n, ok := ExternalFromConvergentKey(k2key); !ok || n != "metrics-sink" {
		t.Fatalf("ExternalFromConvergentKey(k2key) = (%q,%v), want (metrics-sink,true)", n, ok)
	}
	// CONTROL: an external key is NOT mistaken for a per-path key, and the trunk
	// key is recognized as neither a path nor an external per-name key.
	if _, ok := PathFromConvergentKey(k1key); ok {
		t.Fatalf("an external per-name key must NOT be recognized as a per-path key")
	}
	if _, ok := ExternalFromConvergentKey(TrunkKey); ok {
		t.Fatalf("TrunkKey must not be recognized as a per-name external convergent key")
	}
	// CONTROL: a per-path convergent key is NOT mistaken for an external key.
	_, pkey := c.Route(PathRef("a.lock"))
	if _, ok := ExternalFromConvergentKey(pkey); ok {
		t.Fatalf("a per-path key must NOT be recognized as an external per-name key")
	}
}

func TestConvergentPathsGetDistinctPerPathKeys(t *testing.T) {
	c := New(testManifest())
	// Two DISTINCT convergent paths must get DISTINCT keys, neither == TrunkKey,
	// so two agents on different convergent files no longer falsely serialize.
	_, ka := c.Route(PathRef("a.lock"))
	_, kb := c.Route(PathRef("b.lock"))
	if len(ka) == 0 || len(kb) == 0 {
		t.Fatalf("convergent paths must carry a lease key; got %q %q", ka, kb)
	}
	if bytes.Equal(ka, kb) {
		t.Fatalf("distinct convergent paths must get distinct keys; both = %q", ka)
	}
	if bytes.Equal(ka, TrunkKey) || bytes.Equal(kb, TrunkKey) {
		t.Fatalf("a convergent PATH must NOT collapse onto TrunkKey; ka=%q kb=%q", ka, kb)
	}

	// PathFromConvergentKey round-trips the encoded path for display.
	if p, ok := PathFromConvergentKey(ka); !ok || p != "a.lock" {
		t.Fatalf("PathFromConvergentKey(ka) = (%q,%v), want (a.lock,true)", p, ok)
	}
	if p, ok := PathFromConvergentKey(kb); !ok || p != "b.lock" {
		t.Fatalf("PathFromConvergentKey(kb) = (%q,%v), want (b.lock,true)", p, ok)
	}
	// Negative control: the trunk key is NOT a per-path key.
	if _, ok := PathFromConvergentKey(TrunkKey); ok {
		t.Fatalf("TrunkKey must not be recognized as a per-path convergent key")
	}

	// Positive control: non-convergent routes carry no lease key.
	if k, key := c.Route(PathRef("src/x.go")); k != Forkable || key != nil {
		t.Fatalf("forkable must route with no key; got %v key=%q", k, key)
	}
	if k, key := c.Route(ExternalRef("prod-db")); k != Singular || key != nil {
		t.Fatalf("singular must route with no key; got %v key=%q", k, key)
	}
}

// --- Inv 11: declaration, never modification --------------------------------

func TestInitDeclarationNotModification(t *testing.T) {
	dir := t.TempDir()
	// Seed a fake "project".
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n")
	mustMkdir(t, filepath.Join(dir, "src"))
	mustWrite(t, filepath.Join(dir, "src", "a.txt"), "alpha")
	mustWrite(t, filepath.Join(dir, "package.json"), `{"name":"x"}`)

	before := snapshot(t, dir)
	created, err := Init(dir)
	if err != nil || !created {
		t.Fatalf("Init: created=%v err=%v", created, err)
	}
	after := snapshot(t, dir)

	added, modified := diffSnap(before, after)
	if len(added) != 1 || added[0] != ManifestFile {
		t.Fatalf("Init must add EXACTLY %s; added=%v", ManifestFile, added)
	}
	if len(modified) != 0 {
		t.Fatalf("Init must modify ZERO project files; modified=%v", modified)
	}

	// Idempotent: a second Init is a no-op and changes nothing.
	created2, _ := Init(dir)
	if created2 {
		t.Fatal("second Init must be a no-op (created=false)")
	}
	a2, m2 := diffSnap(after, snapshot(t, dir))
	if len(a2) != 0 || len(m2) != 0 {
		t.Fatalf("idempotent Init changed files: added=%v modified=%v", a2, m2)
	}

	// The scaffolded manifest loads and is usable.
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("load scaffolded manifest: %v", err)
	}
	if New(m).Classify(PathRef("migrations/0001.sql")) != Convergent {
		t.Fatal("scaffolded manifest should classify migrations/** as convergent")
	}
}

// --- loader tolerance / fail-closed -----------------------------------------

func TestLoadDefaultsAndTolerance(t *testing.T) {
	// Missing manifest → defaults (path forkable, external singular).
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("missing manifest must yield defaults, got err: %v", err)
	}
	c := New(m)
	if c.Classify(PathRef("x.go")) != Forkable {
		t.Fatal("default: a working-tree path must be forkable")
	}
	if c.Classify(ExternalRef("anything")) != Singular {
		t.Fatal("default: an external resource must be singular (default-deny)")
	}

	// Unknown fields ignored; declared values honored.
	mustWrite(t, filepath.Join(dir, ManifestFile),
		`{"version":1,"future_field":42,"singular":{"resources":["x"]}}`)
	m2, err := Load(dir)
	if err != nil {
		t.Fatalf("unknown fields must be tolerated: %v", err)
	}
	if !m2.SingularResources["x"] {
		t.Fatal("declared singular resource not loaded")
	}

	// Malformed JSON → error (fail-closed; do not run on a broken policy).
	mustWrite(t, filepath.Join(dir, ManifestFile), `{ this is not json `)
	if _, err := Load(dir); err == nil {
		t.Fatal("malformed manifest must fail-closed (error), not silently default")
	}
}

// --- glob matcher -----------------------------------------------------------

func TestGlobMatcher(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"migrations/**", "migrations/0001.sql", true},
		{"migrations/**", "migrations/sub/0002.sql", true},
		{"migrations/**", "src/x.go", false},
		{"**/*.lock", "go.lock", true},
		{"**/*.lock", "a/b/c.lock", true},
		{"**/*.lock", "a/b/c.locks", false},
		{"*.lock", "x.lock", true},
		{"*.lock", "a/x.lock", false}, // single * does not cross a separator
		{"secrets/**", "secrets/api/key.pem", true},
		{"exact/path.go", "exact/path.go", true},
		{"exact/path.go", "exact/other.go", false},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pat, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pat, tc.path, got, tc.want)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func randName(r *rand.Rand) string {
	const alpha = "abcdefghijklmnop/._-"
	n := 1 + r.Intn(12)
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[r.Intn(len(alpha))]
	}
	return string(b)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffSnap(before, after map[string]string) (added, modified []string) {
	for k, v := range after {
		if bv, ok := before[k]; !ok {
			added = append(added, k)
		} else if bv != v {
			modified = append(modified, k)
		}
	}
	sort.Strings(added)
	sort.Strings(modified)
	return added, modified
}
