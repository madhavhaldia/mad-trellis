package integrator

// Origin-bypass escape-resistance (Inv 7) — the mediated-remote half. Two
// structural properties, each with a positive control:
//
//   structural   → TestMediatedRepoRejectsTrunkPush (agent CANNOT push trunk;
//                   +control: an nm/* push IS accepted, and the integrator's
//                   local update-ref DOES advance trunk)
//   auditable    → TestAuditWorktreeRemotes (no configured remote reaches
//                   canonical; +control: a canonical-reaching remote → RED)

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func isolateGitGlobal(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
}

// The protective `update` hook makes "no agent advances trunk by pushing" a
// STRUCTURAL property, not a watched one: a push to the trunk ref is rejected by
// receive-pack; an nm/* push is accepted; and the integrator's LOCAL update-ref
// (the only legitimate trunk advance) is unaffected by receive hooks.
func TestMediatedRepoRejectsTrunkPush(t *testing.T) {
	isolateGitGlobal(t)
	bare := filepath.Join(t.TempDir(), "trunk.git")
	if _, err := EnsureMediatedRepo(bare, "trunk"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	work := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "main", work)
	mustGit(t, work, "config", "user.email", "t@t")
	mustGit(t, work, "config", "user.name", "t")
	mustGit(t, work, "remote", "add", "origin", bare)
	write(t, work, "a.txt", "x\n")
	mustGit(t, work, "add", ".")
	mustGit(t, work, "commit", "-q", "-m", "c")

	// An agent's own nm/* branch push is ACCEPTED.
	if out, err := tryGit(work, "push", "-q", "origin", "HEAD:refs/heads/nm/x"); err != nil {
		t.Fatalf("nm/* push must be accepted: %v\n%s", err, out)
	}

	// A push to the protected trunk ref is REJECTED (the hook denies it).
	out, err := tryGit(work, "push", "origin", "HEAD:refs/heads/trunk")
	if err == nil {
		t.Fatal("FAIL-OPEN: an agent push to trunk was accepted (Inv 7 violated)")
	}
	if !strings.Contains(out, "integrator-only") {
		t.Fatalf("rejection must name the policy; got: %s", out)
	}
	// The trunk ref must not exist in the bare repo after the rejected push.
	if _, err := tryGit(bare, "rev-parse", "--verify", "--quiet", "refs/heads/trunk"); err == nil {
		t.Fatal("the rejected push must not have created the trunk ref")
	}

	// +control: the integrator's LOCAL update-ref DOES advance trunk (receive
	// hooks do not fire for local plumbing) — proving the hook constrains agents,
	// not the integrator.
	commit := revParse(t, work, "HEAD")
	if out, err := tryGit(bare, "update-ref", "refs/heads/trunk", commit); err != nil {
		t.Fatalf("the integrator's local update-ref must advance trunk: %v\n%s", err, out)
	}
}

func TestAuditWorktreeRemotes(t *testing.T) {
	isolateGitGlobal(t)
	bare := filepath.Join(t.TempDir(), "trunk.git")
	if _, err := EnsureMediatedRepo(bare, "trunk"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	canonical := t.TempDir() // stand-in for the REAL trunk / real origin

	work := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "main", work)
	mustGit(t, work, "config", "user.email", "t@t")
	mustGit(t, work, "config", "user.name", "t")
	// Simulate an inherited real origin, then redirect it to the mediated repo.
	mustGit(t, work, "remote", "add", "origin", canonical)
	if err := RedirectRemote(work, bare); err != nil {
		t.Fatalf("redirect: %v", err)
	}

	// CLEAN: after the redirect no configured remote reaches canonical.
	audit, err := AuditWorktreeRemotes(work, []string{canonical})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !audit.Clean {
		t.Fatalf("after redirect the audit must be clean; violations=%+v urls=%+v", audit.Violations, audit.URLs)
	}

	// +control: re-introduce a remote that reaches canonical → the audit MUST
	// flag it (proving the check is non-vacuous).
	mustGit(t, work, "remote", "add", "escape", canonical)
	bad, err := AuditWorktreeRemotes(work, []string{canonical})
	if err != nil {
		t.Fatalf("audit(control): %v", err)
	}
	if bad.Clean {
		t.Fatal("control: a canonical-reaching remote must make the audit RED")
	}
	found := false
	for _, v := range bad.Violations {
		if strings.Contains(v.ConfigKey, "escape") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the violation must name the escaping remote; got %+v", bad.Violations)
	}

	// +control (pushurl escape): `git push` prefers remote.<n>.pushurl, so an
	// inherited pushurl pointing at canonical is a real escape the url-only scan
	// missed — the audit must flag it.
	mustGit(t, work, "remote", "remove", "escape")
	mustGit(t, work, "config", "remote.origin.pushurl", canonical)
	push, err := AuditWorktreeRemotes(work, []string{canonical})
	if err != nil {
		t.Fatalf("audit(pushurl): %v", err)
	}
	if push.Clean {
		t.Fatal("control: a canonical-reaching remote.origin.pushurl must make the audit RED")
	}
}

// RedirectRemote must pin BOTH the fetch url and the PUSH url to the mediated
// repo — an inherited pushurl that survives the redirect is a silent escape.
func TestRedirectRemotePinsPushURL(t *testing.T) {
	isolateGitGlobal(t)
	bare := filepath.Join(t.TempDir(), "trunk.git")
	if _, err := EnsureMediatedRepo(bare, "trunk"); err != nil {
		t.Fatal(err)
	}
	canonical := t.TempDir()
	work := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "main", work)
	mustGit(t, work, "remote", "add", "origin", canonical)
	mustGit(t, work, "config", "remote.origin.pushurl", canonical) // inherited push escape
	if err := RedirectRemote(work, bare); err != nil {
		t.Fatalf("redirect: %v", err)
	}
	bareAbs, _ := filepath.Abs(bare)
	got := mustGit(t, work, "remote", "get-url", "--push", "origin")
	if got != bareAbs {
		t.Fatalf("push url must be redirected to the mediated repo; got %q want %q", got, bareAbs)
	}
	audit, err := AuditWorktreeRemotes(work, []string{canonical})
	if err != nil || !audit.Clean {
		t.Fatalf("after redirect no target may reach canonical; clean=%v err=%v violations=%+v", audit.Clean, err, audit.Violations)
	}
}

func TestEnsureMediatedRepoIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trunk.git")
	a, err := EnsureMediatedRepo(dir, "trunk")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureMediatedRepo(dir, "trunk") // second call must not fail or wipe state
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("idempotent ensure must return the same path; %s != %s", a, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooks", "update")); err != nil {
		t.Fatalf("the protective update hook must be installed: %v", err)
	}
}

func tryGit(dir string, args ...string) (string, error) {
	var full []string
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	c := exec.Command("git", full...)
	out, err := c.CombinedOutput()
	return string(out), err
}
