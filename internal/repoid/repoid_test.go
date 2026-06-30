package repoid

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "t@t")
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestCanonicalIsAbsoluteGitDir(t *testing.T) {
	repo := initRepo(t)
	got := Canonical(repo)
	if got == "" {
		t.Fatal("Canonical must never be empty")
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("Canonical must be absolute, got %q", got)
	}
	if filepath.Base(got) != ".git" {
		t.Fatalf("Canonical of a normal repo should end in .git, got %q", got)
	}
}

// THE key property (C39-class): a main checkout and a LINKED worktree of the
// same repo resolve to ONE canonical identity.
func TestCanonicalSharedAcrossCheckouts(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "linked")
	gitRun(t, repo, "worktree", "add", "-b", "wt-test", wt, "HEAD")

	main := Canonical(repo)
	linked := Canonical(wt)
	if main == "" || linked == "" {
		t.Fatalf("both must resolve: main=%q linked=%q", main, linked)
	}
	if main != linked {
		t.Fatalf("two checkouts of one repo must share identity: main=%q linked=%q", main, linked)
	}
}

// A non-repo dir falls back to its own absolute path — deterministic, non-empty,
// exactly the old per-path behavior.
func TestCanonicalNonRepoFallback(t *testing.T) {
	dir := t.TempDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := Canonical(dir)
	if got != abs {
		t.Fatalf("non-repo fallback = %q, want abs path %q", got, abs)
	}
}
