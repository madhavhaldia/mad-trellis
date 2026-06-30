package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madhavhaldia/mad-substrate/internal/worktree"
)

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p) // resolve /var→/private/var so git + worktree.Path agree
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func initRepo(t *testing.T) string {
	t.Helper()
	root := mustEval(t, t.TempDir())
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t")
	gitT(t, root, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "add", ".")
	gitT(t, root, "commit", "-q", "-m", "base")
	return root
}

func chdirT(t *testing.T, dir string) {
	t.Helper()
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func addSpawnWorktree(t *testing.T, repo, slug string) string {
	t.Helper()
	path, err := worktree.Path(repo, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "worktree", "add", "-q", "-b", "nm/"+slug, path, "HEAD")
	return path
}

func runDespawn(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := despawnCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func branchExists(repo, branch string) bool {
	return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

func TestDespawnRemovesCleanIntegratedWorktree(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("MAD_WORKTREE_DIR", mustEval(t, t.TempDir()))
	t.Setenv("MAD_STATE_DIR", mustEval(t, t.TempDir()))
	chdirT(t, repo)

	slug := "s-1-cleanbeef"
	path := addSpawnWorktree(t, repo, slug)
	// nm/<slug> == HEAD (no new commits) and clean → integrated → despawn removes it.
	out, err := runDespawn(t, "nm/"+slug)
	if err != nil {
		t.Fatalf("despawn: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("worktree not removed:\n%s", out)
	}
	if branchExists(repo, "nm/"+slug) {
		t.Fatalf("branch not removed:\n%s", out)
	}
}

func TestDespawnRefusesUncommittedThenForce(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("MAD_WORKTREE_DIR", mustEval(t, t.TempDir()))
	t.Setenv("MAD_STATE_DIR", mustEval(t, t.TempDir()))
	chdirT(t, repo)

	slug := "s-1-dirtybeef"
	path := addSpawnWorktree(t, repo, slug)
	if err := os.WriteFile(filepath.Join(path, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := runDespawn(t, slug); err == nil { // bare slug accepted
		t.Fatalf("expected refusal on uncommitted work; out=%s", out)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("worktree wrongly removed despite refusal")
	}
	if out, err := runDespawn(t, slug, "--force"); err != nil {
		t.Fatalf("--force despawn: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("worktree not removed with --force")
	}
}

func TestDespawnRefusesUnintegratedThenForce(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("MAD_WORKTREE_DIR", mustEval(t, t.TempDir()))
	t.Setenv("MAD_STATE_DIR", mustEval(t, t.TempDir()))
	chdirT(t, repo)

	slug := "s-1-aheadbeef"
	path := addSpawnWorktree(t, repo, slug)
	// Commit in the worktree → nm/<slug> is AHEAD of main HEAD (unintegrated).
	if err := os.WriteFile(filepath.Join(path, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, path, "add", ".")
	gitT(t, path, "commit", "-q", "-m", "work")

	if out, err := runDespawn(t, "nm/"+slug); err == nil {
		t.Fatalf("expected refusal on unintegrated commits; out=%s", out)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("worktree wrongly removed despite refusal")
	}
	if out, err := runDespawn(t, "nm/"+slug, "--force"); err != nil {
		t.Fatalf("--force despawn: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("worktree not removed with --force")
	}
}

// TestDespawnRefusesDivergedHeadThenForce is the regression for the review's HIGH:
// when the worktree's checked-out HEAD has moved OFF its branch (an integrated
// commit) while nm/<slug> still holds unintegrated commits, despawn must gate on
// the BRANCH tip (refuse), not the worktree HEAD (which would falsely pass).
func TestDespawnRefusesDivergedHeadThenForce(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("MAD_WORKTREE_DIR", mustEval(t, t.TempDir()))
	t.Setenv("MAD_STATE_DIR", mustEval(t, t.TempDir()))
	chdirT(t, repo)

	slug := "s-1-divergebeef"
	path := addSpawnWorktree(t, repo, slug)
	if err := os.WriteFile(filepath.Join(path, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, path, "add", ".")
	gitT(t, path, "commit", "-q", "-m", "work")     // nm/<slug> now ahead (unintegrated)
	gitT(t, path, "checkout", "--detach", "HEAD~1") // move worktree HEAD to the integrated base

	if out, err := runDespawn(t, "nm/"+slug); err == nil {
		t.Fatalf("expected refusal: nm/%s has unintegrated commits even though the worktree HEAD is detached; out=%s", slug, out)
	}
	if !branchExists(repo, "nm/"+slug) {
		t.Fatal("branch wrongly deleted despite refusal (DATA LOSS)")
	}
	if out, err := runDespawn(t, "nm/"+slug, "--force"); err != nil {
		t.Fatalf("--force despawn: %v\n%s", err, out)
	}
	if branchExists(repo, "nm/"+slug) {
		t.Fatal("branch not removed with --force")
	}
}

// TestDespawnRefusesDirAbsentUnintegratedThenForce is the regression for the
// review's HIGH: the safety gate must run even when the worktree DIR is gone but
// the unintegrated nm/<slug> branch still exists in the canonical repo.
func TestDespawnRefusesDirAbsentUnintegratedThenForce(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("MAD_WORKTREE_DIR", mustEval(t, t.TempDir()))
	t.Setenv("MAD_STATE_DIR", mustEval(t, t.TempDir()))
	chdirT(t, repo)

	slug := "s-1-offdiskbeef"
	path := addSpawnWorktree(t, repo, slug)
	if err := os.WriteFile(filepath.Join(path, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, path, "add", ".")
	gitT(t, path, "commit", "-q", "-m", "work")          // nm/<slug> ahead (unintegrated)
	gitT(t, repo, "worktree", "remove", "--force", path) // dir gone, branch stays in canonical
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("setup: worktree dir should be gone")
	}
	if !branchExists(repo, "nm/"+slug) {
		t.Fatal("setup: the unintegrated branch should still exist")
	}

	if out, err := runDespawn(t, "nm/"+slug); err == nil {
		t.Fatalf("expected refusal: nm/%s is unintegrated even though its worktree dir is gone; out=%s", slug, out)
	}
	if !branchExists(repo, "nm/"+slug) {
		t.Fatal("branch wrongly deleted despite refusal (DATA LOSS)")
	}
	if out, err := runDespawn(t, "nm/"+slug, "--force"); err != nil {
		t.Fatalf("--force despawn: %v\n%s", err, out)
	}
	if branchExists(repo, "nm/"+slug) {
		t.Fatal("branch not removed with --force")
	}
}

// TestDespawnRefusesForeignLockThenForceUnlock is R14: a boundary worktree LOCKED by
// a foreign tool (e.g. Supacode: `git worktree lock --reason owner:supacode`) must NOT
// be force-removed by default. Despawn must REFUSE, report the lock reason, and leave
// the worktree on disk (the non-vacuous control). `--force-unlock` then unlocks and
// removes it.
func TestDespawnRefusesForeignLockThenForceUnlock(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("MAD_WORKTREE_DIR", mustEval(t, t.TempDir()))
	t.Setenv("MAD_STATE_DIR", mustEval(t, t.TempDir()))
	chdirT(t, repo)

	slug := "s-1-lockedbeef"
	path := addSpawnWorktree(t, repo, slug)
	// Clean + integrated (nm/<slug> == HEAD) so the ONLY thing blocking removal is the
	// foreign lock — isolates the lock behavior from the work-safety gate.
	gitT(t, repo, "worktree", "lock", "--reason", "owner:supacode", "--", path)

	// DEFAULT: refuse, report the reason, and the worktree still EXISTS (the control).
	out, err := runDespawn(t, "nm/"+slug)
	if err == nil {
		t.Fatalf("expected refusal on a foreign-locked worktree; out=%s", out)
	}
	if !strings.Contains(err.Error(), "LOCKED") || !strings.Contains(err.Error(), "owner:supacode") {
		t.Fatalf("error must report the lock + reason, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--force-unlock") {
		t.Fatalf("error must point at --force-unlock, got: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("worktree wrongly removed despite the lock (must not force by default)")
	}
	if !branchExists(repo, "nm/"+slug) {
		t.Fatal("branch wrongly deleted despite refusal (no partial state allowed)")
	}

	// Plain --force (the work-discard flag) must NOT override a FOREIGN lock — only
	// --force-unlock does. Proves the two overrides are distinct.
	if out, err := runDespawn(t, "nm/"+slug, "--force"); err == nil {
		t.Fatalf("--force (discard) must not break a foreign lock; out=%s", out)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("worktree wrongly removed by plain --force despite the lock")
	}

	// --force-unlock: unlock and remove despite the lock.
	out, err = runDespawn(t, "nm/"+slug, "--force-unlock")
	if err != nil {
		t.Fatalf("--force-unlock despawn: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("worktree not removed with --force-unlock:\n%s", out)
	}
	if branchExists(repo, "nm/"+slug) {
		t.Fatalf("branch not removed with --force-unlock:\n%s", out)
	}
	if !strings.Contains(out, "force-unlocked") {
		t.Fatalf("output should report the force-unlock, got:\n%s", out)
	}
}

// TestDespawnUnlockedIgnoresForceUnlock proves --force-unlock is inert on an UNLOCKED
// worktree: it despawns exactly as today, with or without the flag.
func TestDespawnUnlockedIgnoresForceUnlock(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("MAD_WORKTREE_DIR", mustEval(t, t.TempDir()))
	t.Setenv("MAD_STATE_DIR", mustEval(t, t.TempDir()))
	chdirT(t, repo)

	slug := "s-1-unlockedbeef"
	path := addSpawnWorktree(t, repo, slug)
	out, err := runDespawn(t, "nm/"+slug, "--force-unlock")
	if err != nil {
		t.Fatalf("despawn --force-unlock on an unlocked worktree: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("unlocked worktree not removed:\n%s", out)
	}
	if branchExists(repo, "nm/"+slug) {
		t.Fatalf("branch not removed:\n%s", out)
	}
	if strings.Contains(out, "force-unlocked") {
		t.Fatalf("must NOT report force-unlock for an unlocked worktree, got:\n%s", out)
	}
}

func TestDespawnRejectsUnsafeSlug(t *testing.T) {
	repo := initRepo(t)
	chdirT(t, repo)
	for _, bad := range []string{"../etc", "a/b", "nm/../x", ""} {
		if out, err := runDespawn(t, bad); err == nil {
			t.Fatalf("unsafe id %q should be rejected; out=%s", bad, out)
		}
	}
}
