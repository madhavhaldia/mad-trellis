package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs git in dir, failing the test on error, returning trimmed output.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v: %s", dir, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedRepo creates a git repo with one commit on a deterministic default branch.
func seedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.email", "t@t")
	gitT(t, dir, "config", "user.name", "t")
	writeFileT(t, filepath.Join(dir, "base.txt"), "base\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "base")
	return dir
}

// TestIntegrateMergeWorktreeGrain: the default (from=="") path merges a local
// branch into the current branch — the worktree-grain dogfood loop.
func TestIntegrateMergeWorktreeGrain(t *testing.T) {
	repo := seedRepo(t)
	// A feature branch with a commit, then back to main.
	gitT(t, repo, "checkout", "-q", "-b", "feature")
	writeFileT(t, filepath.Join(repo, "feature.txt"), "feat\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "feature work")
	gitT(t, repo, "checkout", "-q", "main")

	if err := integrateMerge(repo, "", "feature"); err != nil {
		t.Fatalf("integrateMerge (worktree grain): %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("the feature commit must be merged into main: %v", err)
	}
}

// TestIntegrateMergeWorktreeGrainViaLinkedWorktree: the real worktree-grain
// convergence path. A boundary branch is created in a LINKED worktree (git
// worktree add) — exactly how `mad-substrate spawn` runs an agent. Because a linked
// worktree shares the parent repo's `.git`, its branch+commits are already in
// the shared object store, so `integrateMerge` (from=="") resolves and merges
// the branch from the MAIN worktree with NO publish step (no push, no separate
// trunk.git). This is what makes worktree-grain converge with no publish gap.
func TestIntegrateMergeWorktreeGrainViaLinkedWorktree(t *testing.T) {
	repo := seedRepo(t)

	// Spawn-like: a linked worktree on a boundary branch nm/<slug>, with a commit.
	wt := filepath.Join(t.TempDir(), "boundary")
	gitT(t, repo, "worktree", "add", "-q", "-b", "nm/s-1-abc", wt, "main")
	writeFileT(t, filepath.Join(wt, "boundary.txt"), "boundary work\n")
	gitT(t, wt, "add", ".")
	gitT(t, wt, "commit", "-q", "-m", "boundary work")

	// The shared .git already has the branch — no fetch/--from needed. Integrate
	// from the MAIN worktree resolves nm/s-1-abc and merges it into main.
	if err := integrateMerge(repo, "", "nm/s-1-abc"); err != nil {
		t.Fatalf("integrateMerge of a linked-worktree branch: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "boundary.txt")); err != nil || string(got) != "boundary work\n" {
		t.Fatalf("the boundary branch's commit must be merged into main (got %q, err %v)", got, err)
	}
}

// TestIntegrateMergeWorktreeGrainConflictAbortsCleanly: a conflicting boundary
// branch (created in a linked worktree, the spawn shape) aborts cleanly, leaving
// the target branch byte-identical — the gate never corrupts the trunk.
func TestIntegrateMergeWorktreeGrainConflictAbortsCleanly(t *testing.T) {
	repo := seedRepo(t)

	// Linked worktree off the ORIGINAL base, edits base.txt one way.
	wt := filepath.Join(t.TempDir(), "boundary")
	gitT(t, repo, "worktree", "add", "-q", "-b", "nm/s-2-rival", wt, "main")
	writeFileT(t, filepath.Join(wt, "base.txt"), "rival-change\n")
	gitT(t, wt, "commit", "-qam", "rival change")

	// main moves the same line the other way, creating a true conflict.
	writeFileT(t, filepath.Join(repo, "base.txt"), "main-change\n")
	gitT(t, repo, "commit", "-qam", "main change")
	mainTip := gitT(t, repo, "rev-parse", "HEAD")

	if err := integrateMerge(repo, "", "nm/s-2-rival"); err == nil {
		t.Fatal("a conflicting boundary integration must return an error")
	}
	if tip := gitT(t, repo, "rev-parse", "HEAD"); tip != mainTip {
		t.Fatalf("conflict must leave the target byte-identical: %q != %q", tip, mainTip)
	}
	if status := gitT(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("conflict must abort to a clean tree, got:\n%s", status)
	}
}

// TestIntegrateMergeFromClone: the container-grain --from path fetches the branch
// from a SEPARATE clone (distinct object store) and merges it — the commit was
// never in the canonical repo until --from brought it over.
func TestIntegrateMergeFromClone(t *testing.T) {
	repo := seedRepo(t)
	// A standalone clone with an agent branch + commit (no shared objects).
	clone := t.TempDir()
	gitT(t, clone, "clone", "-q", "--no-hardlinks", repo, ".")
	gitT(t, clone, "config", "user.email", "a@a")
	gitT(t, clone, "config", "user.name", "a")
	gitT(t, clone, "checkout", "-q", "-b", "nm/agent")
	writeFileT(t, filepath.Join(clone, "agent.txt"), "agentwork\n")
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-q", "-m", "agent work")

	// Precondition: the canonical repo does not have the branch.
	if err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "nm/agent").Run(); err == nil {
		t.Fatal("precondition: canonical repo must NOT have nm/agent before --from")
	}

	if err := integrateMerge(repo, clone, "nm/agent"); err != nil {
		t.Fatalf("integrateMerge --from clone: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "agent.txt")); err != nil || string(got) != "agentwork\n" {
		t.Fatalf("the clone's commit must be merged into the canonical repo (got %q, err %v)", got, err)
	}
}

// TestIntegrateMergeConflictAbortsCleanly: a conflicting merge is aborted so the
// repo is left clean (the trunk is byte-identical), and an error is returned.
func TestIntegrateMergeConflictAbortsCleanly(t *testing.T) {
	repo := seedRepo(t)
	// main edits base.txt one way...
	writeFileT(t, filepath.Join(repo, "base.txt"), "main-change\n")
	gitT(t, repo, "commit", "-qam", "main change")
	mainTip := gitT(t, repo, "rev-parse", "HEAD")
	// ...a branch off the ORIGINAL base edits the same line the other way.
	gitT(t, repo, "checkout", "-q", "-b", "rival", "HEAD~1")
	writeFileT(t, filepath.Join(repo, "base.txt"), "rival-change\n")
	gitT(t, repo, "commit", "-qam", "rival change")
	gitT(t, repo, "checkout", "-q", "main")

	if err := integrateMerge(repo, "", "rival"); err == nil {
		t.Fatal("a conflicting merge must return an error")
	}
	// The merge was aborted: HEAD is unchanged and the tree is clean.
	if tip := gitT(t, repo, "rev-parse", "HEAD"); tip != mainTip {
		t.Fatalf("conflict must leave HEAD byte-identical: %q != %q", tip, mainTip)
	}
	if status := gitT(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("conflict must abort to a clean tree, got:\n%s", status)
	}
}

// TestIntegrateMergeFromMissingPathErrors: a --from path that is not a repo fails
// cleanly (no partial merge).
func TestIntegrateMergeFromMissingPathErrors(t *testing.T) {
	repo := seedRepo(t)
	if err := integrateMerge(repo, filepath.Join(t.TempDir(), "nope"), "nm/agent"); err == nil {
		t.Fatal("integrateMerge with a bogus --from path must error")
	}
}

// TestIntegrateMergeRejectsArgInjection: a branch or --from value that could be
// parsed by git as a FLAG is rejected BEFORE any git command runs (no merge/abort,
// trunk untouched).
func TestIntegrateMergeRejectsArgInjection(t *testing.T) {
	repo := seedRepo(t)
	tip := gitT(t, repo, "rev-parse", "HEAD")

	// (1) A flag-shaped branch is rejected (git merge -D would be a flag).
	for _, bad := range []string{"-D", "--allow-empty", "", "has space", "a;b"} {
		if err := integrateMerge(repo, "", bad); err == nil {
			t.Fatalf("integrateMerge must reject unsafe branch %q", bad)
		}
	}
	// (2) A flag-shaped --from is rejected (git fetch --upload-pack=... would be a flag).
	if err := integrateMerge(repo, "--upload-pack=evil", "nm/agent"); err == nil {
		t.Fatal("integrateMerge must reject a flag-shaped --from")
	}
	// Trunk untouched by any rejected call.
	if got := gitT(t, repo, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("rejected integrate must leave HEAD byte-identical: %q != %q", got, tip)
	}
}

// TestIsSafeBranchName: the validator accepts nm/<slug> and rejects flag-shaped /
// illegal names.
func TestIsSafeBranchName(t *testing.T) {
	for _, ok := range []string{"nm/s-1-abc", "feature", "release/1.2.3", "a_b-c"} {
		if !isSafeBranchName(ok) {
			t.Fatalf("isSafeBranchName(%q) should be true", ok)
		}
	}
	for _, bad := range []string{"", "-D", "--flag", "a b", "a;b", "a$b", "a\nb"} {
		if isSafeBranchName(bad) {
			t.Fatalf("isSafeBranchName(%q) should be false", bad)
		}
	}
}
