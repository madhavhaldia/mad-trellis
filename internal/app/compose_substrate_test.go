package app

// Integration test: the P2 substrate over the Unix socket against the composed,
// frozen daemon. Proves substrate.provision/teardown work end to end and that
// the boundary is bound to the CALLER's connection identity (Inv 4) — a
// different session cannot tear down another's boundary.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "init"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestSubstrateRoundTrip(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	repo := initGitRepo(t)

	sock := shortSock(t)
	d, closeLedger, err := Build(Config{
		SocketPath: sock,
		LedgerPath: filepath.Join(t.TempDir(), "ledger.db"),
		RepoRoot:   repo,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer closeLedger()
	if err := d.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _ = d.Serve() }()
	defer d.Close()

	a := dialc(t, sock)
	defer a.close()

	var spec struct {
		Session string            `json:"session"`
		Grain   string            `json:"grain"`
		Cwd     string            `json:"cwd"`
		Branch  string            `json:"branch"`
		Ports   []int             `json:"ports"`
		Env     map[string]string `json:"env"`
	}
	resultOf(t, a.call(t, "substrate.provision", map[string]any{}), &spec)

	if spec.Grain != "worktree" {
		t.Fatalf("v1 grain must be worktree, got %q", spec.Grain)
	}
	if _, err := os.Stat(spec.Cwd); err != nil {
		t.Fatalf("provisioned worktree must exist: %v", err)
	}
	if len(spec.Ports) == 0 || spec.Env["PORT"] == "" {
		t.Fatalf("env-spec must carry a per-agent port block: %+v", spec)
	}

	// Inv 4 (NON-VACUOUS): a SECOND connection provisions its OWN distinct boundary;
	// each teardown is scoped to the caller's connection-bound session, so b's
	// teardown removes ONLY b's boundary and leaves a's intact — proving the
	// boundary is keyed off the connection identity, not a shared/targetable one.
	b := dialc(t, sock)
	defer b.close()
	var specB struct {
		Cwd string `json:"cwd"`
	}
	resultOf(t, b.call(t, "substrate.provision", map[string]any{}), &specB)
	if specB.Cwd == "" || specB.Cwd == spec.Cwd {
		t.Fatalf("second session must get its own distinct boundary; got %q vs %q", specB.Cwd, spec.Cwd)
	}

	var bok struct {
		OK bool `json:"ok"`
	}
	resultOf(t, b.call(t, "substrate.teardown", map[string]any{}), &bok)
	if _, err := os.Stat(specB.Cwd); !os.IsNotExist(err) {
		t.Fatal("b's teardown must remove b's own boundary")
	}
	if _, err := os.Stat(spec.Cwd); err != nil {
		t.Fatal("Inv 4 VIOLATED: b's teardown removed a's boundary")
	}

	// The owner tears down its own boundary.
	var aok struct {
		OK bool `json:"ok"`
	}
	resultOf(t, a.call(t, "substrate.teardown", map[string]any{}), &aok)
	if !aok.OK {
		t.Fatal("owner teardown must succeed")
	}
	if _, err := os.Stat(spec.Cwd); !os.IsNotExist(err) {
		t.Fatal("owner teardown must remove the worktree")
	}
}
