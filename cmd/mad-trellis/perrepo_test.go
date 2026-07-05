package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/madhavhaldia/mad-trellis/internal/runtimecfg"
)

func resetPerRepo(t *testing.T) {
	perRepoRuntimeRoot = ""
	t.Cleanup(func() { perRepoRuntimeRoot = "" })
}

// applyAndRuntimeDir runs the auto-default from inside dir and returns the
// exported MAD_RUNTIME_DIR. Clears every override first.
func applyAndRuntimeDir(t *testing.T, dir string) string {
	t.Helper()
	resetPerRepo(t)
	chdirT(t, dir)
	t.Setenv("MAD_RUNTIME_DIR", "")
	t.Setenv("MAD_HOME", "")
	t.Setenv("MAD_SOCKET", "")
	applyPerRepoRuntimeDefault()
	return os.Getenv("MAD_RUNTIME_DIR")
}

// in a git repo with no override → per-repo runtime is exported, keyed by the
// repo's canonical (shared git common dir) identity.
func TestApplyPerRepoRuntimeDefault_InRepo(t *testing.T) {
	resetPerRepo(t)
	repo := initRepo(t) // a temp git repo, symlink-resolved
	chdirT(t, repo)
	t.Setenv("MAD_RUNTIME_DIR", "")
	t.Setenv("MAD_HOME", "")
	t.Setenv("MAD_SOCKET", "")

	applyPerRepoRuntimeDefault()

	// The identity is the repo's git common dir (its .git), not the worktree path.
	commonDir := mustEval(t, filepath.Join(repo, ".git"))
	want := runtimecfg.PerRepoRuntimeDir(commonDir)
	if got := os.Getenv("MAD_RUNTIME_DIR"); got != want {
		t.Fatalf("MAD_RUNTIME_DIR = %q, want per-repo %q", got, want)
	}
	if perRepoRuntimeRoot != commonDir {
		t.Fatalf("perRepoRuntimeRoot = %q, want %q", perRepoRuntimeRoot, commonDir)
	}
}

// THE BUG FIX: two distinct working paths that are linked worktrees of the SAME
// repo (shared .git) must resolve to the SAME per-repo runtime dir.
func TestApplyPerRepoRuntimeDefault_SharedAcrossWorktrees(t *testing.T) {
	repo := initRepo(t)
	// A linked worktree of the same repo, checked out at a different path.
	linkedPath := filepath.Join(t.TempDir(), "linked")
	gitT(t, repo, "worktree", "add", "-q", "-b", "feat", linkedPath, "HEAD")
	linked := mustEval(t, linkedPath) // resolve only after git creates the dir

	main := applyAndRuntimeDir(t, repo)
	other := applyAndRuntimeDir(t, linked)

	if main == "" {
		t.Fatal("main worktree must resolve to a per-repo runtime dir")
	}
	if main != other {
		t.Fatalf("two worktrees of one repo must share a runtime dir: main=%q linked=%q", main, other)
	}
}

// two genuinely DIFFERENT repos must resolve to DIFFERENT per-repo runtime dirs.
func TestApplyPerRepoRuntimeDefault_DistinctRepos(t *testing.T) {
	repoA := initRepo(t)
	repoB := initRepo(t)

	a := applyAndRuntimeDir(t, repoA)
	b := applyAndRuntimeDir(t, repoB)

	if a == "" || b == "" {
		t.Fatalf("both repos must resolve: a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatalf("distinct repos must get distinct runtime dirs, both = %q", a)
	}
}

// any explicit runtime/home/socket override suppresses per-repo (a no-op).
func TestApplyPerRepoRuntimeDefault_RespectsOverrides(t *testing.T) {
	repo := initRepo(t)
	chdirT(t, repo)
	for _, ov := range []string{"MAD_RUNTIME_DIR", "MAD_HOME", "MAD_SOCKET"} {
		t.Run(ov, func(t *testing.T) {
			resetPerRepo(t)
			t.Setenv("MAD_RUNTIME_DIR", "")
			t.Setenv("MAD_HOME", "")
			t.Setenv("MAD_SOCKET", "")
			t.Setenv(ov, "/explicit/override")

			applyPerRepoRuntimeDefault()

			if perRepoRuntimeRoot != "" {
				t.Fatalf("%s set → per-repo must be a no-op; perRepoRuntimeRoot=%q", ov, perRepoRuntimeRoot)
			}
			if ov != "MAD_RUNTIME_DIR" && os.Getenv("MAD_RUNTIME_DIR") != "" {
				t.Fatalf("%s set → must not export MAD_RUNTIME_DIR; got %q", ov, os.Getenv("MAD_RUNTIME_DIR"))
			}
		})
	}
}

// outside any git repo → no-op; the global ~/.mad-trellis default stands.
func TestApplyPerRepoRuntimeDefault_NotInRepo(t *testing.T) {
	resetPerRepo(t)
	dir := mustEval(t, t.TempDir()) // a plain dir, not a git repo
	chdirT(t, dir)
	t.Setenv("MAD_RUNTIME_DIR", "")
	t.Setenv("MAD_HOME", "")
	t.Setenv("MAD_SOCKET", "")

	applyPerRepoRuntimeDefault()

	if perRepoRuntimeRoot != "" {
		t.Fatalf("outside a repo → no-op; perRepoRuntimeRoot=%q", perRepoRuntimeRoot)
	}
	if os.Getenv("MAD_RUNTIME_DIR") != "" {
		t.Fatalf("outside a repo → must not set MAD_RUNTIME_DIR; got %q", os.Getenv("MAD_RUNTIME_DIR"))
	}
}
