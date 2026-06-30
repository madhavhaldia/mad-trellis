package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q")
	run(t, dir, "git", "config", "user.email", "t@t")
	run(t, dir, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-q", "-m", "init")
	return dir
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func TestCreateIsolatedWorktrees(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir()) // keep worktrees out of $HOME during tests
	repo := initRepo(t)
	a, err := Create(repo, "s-1-aaaa")
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := Create(repo, "s-2-bbbb")
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	if a.Branch == b.Branch || a.Path == b.Path {
		t.Fatalf("worktrees must be distinct: %+v / %+v", a, b)
	}
	// Inv 1: a file written in A's worktree is invisible in B's.
	if err := os.WriteFile(filepath.Join(a.Path, "x.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.Path, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("B must not see A's file (isolation)")
	}
	if err := Remove(repo, a.Path); err != nil {
		t.Fatalf("remove A: %v", err)
	}
	if err := Remove(repo, b.Path); err != nil {
		t.Fatalf("remove B: %v", err)
	}
}

// Two checkouts of ONE repo (a main checkout and a LINKED git worktree) must
// share the SAME worktreeBase — storage is keyed on the canonical git identity,
// not the per-checkout path. Without this, a daemon restart from a different
// checkout derives a different base and cannot reclaim the old boundary (Inv 3).
func TestWorktreeBaseSharedAcrossCheckouts(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	repo := initRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, repo, "git", "worktree", "add", "-b", "wt-test", linked, "HEAD")

	mainAbs, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	linkedAbs, err := filepath.Abs(linked)
	if err != nil {
		t.Fatal(err)
	}
	if worktreeBase(mainAbs) != worktreeBase(linkedAbs) {
		t.Fatalf("two checkouts must share worktreeBase: main=%q linked=%q",
			worktreeBase(mainAbs), worktreeBase(linkedAbs))
	}
}

// An unsafe name (path traversal / git-ref-unsafe) is rejected, never silently
// rewritten — the substrate must hand a pre-sanitized session slug.
func TestCreateRejectsUnsafeName(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	repo := initRepo(t)
	for _, bad := range []string{"", "../escape", "a/b", "has space", "dot.dot"} {
		if _, err := Create(repo, bad); err == nil {
			t.Fatalf("Create must reject unsafe name %q", bad)
		}
	}
}
