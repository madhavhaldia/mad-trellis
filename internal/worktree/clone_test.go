package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runOut runs git in dir and returns its trimmed combined output (a capturing
// sibling of the test's run helper).
func runOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCreateCloneIsSelfContainedAndIntegrable is the item #3 contract: the
// container grain's clone must be a SELF-CONTAINED repo (so an agent whose only
// mount is this dir can commit), with no origin and a default identity, and a
// commit made in it must be HARVESTABLE into the canonical repo (the basis for
// `integrate --from` and harvest-on-teardown).
func TestCreateCloneIsSelfContainedAndIntegrable(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	repo := initRepo(t)

	wt, err := CreateClone(repo, "s-1-clone")
	if err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	if wt.Branch != "nm/s-1-clone" {
		t.Fatalf("clone branch must be nm/s-1-clone, got %q", wt.Branch)
	}

	// (1) SELF-CONTAINED: <path>/.git is a real DIRECTORY (a linked worktree's .git
	// is a FILE pointing into the canonical repo, which is unusable inside a
	// container mount). The objects are copied in, so git works with no other path.
	gitDir := filepath.Join(wt.Path, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		t.Fatalf("clone must have a .git: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("clone's .git must be a DIRECTORY (self-contained), not a worktree pointer file")
	}
	// The cloned content is present.
	if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
		t.Fatalf("clone must contain the repo's seed file: %v", err)
	}

	// (2) No origin (the agent works purely locally; integration is host-mediated).
	if remotes := runOut(t, wt.Path, "remote"); remotes != "" {
		t.Fatalf("clone must have NO remote (origin removed), got %q", remotes)
	}
	// (3) A default identity is set so commits succeed with no image git config.
	if email := runOut(t, wt.Path, "config", "user.email"); email != "mad-substrate-agent@local" {
		t.Fatalf("clone must set a default user.email, got %q", email)
	}

	// (4) COMMIT in the clone (the in-container agent's action, here host-side via the
	// same working dir the bind mount exposes) and HARVEST it into the canonical repo.
	if err := os.WriteFile(filepath.Join(wt.Path, "agent.txt"), []byte("from-agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, wt.Path, "git", "add", "agent.txt")
	run(t, wt.Path, "git", "commit", "-q", "-m", "agent commit")

	// The canonical repo does NOT have the branch yet (separate object store).
	if err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "nm/s-1-clone").Run(); err == nil {
		t.Fatal("precondition: the canonical repo must NOT have the clone's branch before harvest")
	}
	// Harvest (the mechanism behind `integrate --from` / harvest-on-teardown).
	runOut(t, repo, "fetch", "--no-tags", wt.Path, "+nm/s-1-clone:nm/s-1-clone")
	if got := runOut(t, repo, "show", "nm/s-1-clone:agent.txt"); got != "from-agent" {
		t.Fatalf("the agent's commit must be harvestable into the canonical repo, got %q", got)
	}
}

// TestCreateCloneIdempotentOverStaleDir: a stale clone dir at the deterministic
// path (a crashed prior provision) does not block re-provision — CreateClone clears
// it and re-clones.
func TestCreateCloneIdempotentOverStaleDir(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	repo := initRepo(t)
	first, err := CreateClone(repo, "s-2-stale")
	if err != nil {
		t.Fatalf("CreateClone (first): %v", err)
	}
	// Leave a marker so we can prove the dir was actually re-created.
	if err := os.WriteFile(filepath.Join(first.Path, "STALE_MARKER"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := CreateClone(repo, "s-2-stale")
	if err != nil {
		t.Fatalf("CreateClone (re-provision over stale): %v", err)
	}
	if second.Path != first.Path {
		t.Fatalf("re-provision must reuse the deterministic path: %q vs %q", first.Path, second.Path)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "STALE_MARKER")); !os.IsNotExist(err) {
		t.Fatal("re-provision must have CLEARED the stale clone (marker should be gone)")
	}
}

// TestCreateCloneRejectsUnsafeName: like Create, the clone path/branch is keyed off
// a pre-sanitized slug; an unsafe name is rejected, never rewritten.
func TestCreateCloneRejectsUnsafeName(t *testing.T) {
	t.Setenv("MAD_WORKTREE_DIR", t.TempDir())
	repo := initRepo(t)
	for _, bad := range []string{"", "../escape", "a/b", "has space"} {
		if _, err := CreateClone(repo, bad); err == nil {
			t.Fatalf("CreateClone must reject unsafe name %q", bad)
		}
	}
}
