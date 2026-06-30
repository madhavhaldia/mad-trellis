package conductor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRPC implements RPCClient against canned daemon responses and records the
// sequence of methods called so a test can assert lease.release ran.
type fakeRPC struct {
	mu sync.Mutex
	// denyAcquires: for the first N lease.acquire calls, return granted=false.
	denyAcquires int
	calls        []string
}

func (f *fakeRPC) Call(method string, params any, result any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	deny := false
	if method == "lease.acquire" && f.denyAcquires > 0 {
		f.denyAcquires--
		deny = true
	}
	f.mu.Unlock()

	var canned any
	switch method {
	case "classify.route":
		canned = map[string]any{"lease_key": "dGVzdA=="}
	case "lease.acquire":
		if deny {
			canned = map[string]any{"granted": false, "holder": "other-session"}
		} else {
			canned = map[string]any{"granted": true}
		}
	case "lease.renew", "lease.release":
		canned = map[string]any{"ok": true}
	default:
		canned = map[string]any{}
	}
	if result == nil {
		return nil
	}
	// Robust generic decode: marshal the canned map, unmarshal into the caller's
	// result pointer (mirrors what a real JSON-RPC client does).
	b, err := json.Marshal(canned)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, result)
}

func (f *fakeRPC) recorded(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.calls {
		if m == method {
			return true
		}
	}
	return false
}

// run executes a command in dir and fails the test on error.
func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates a temp git repo on branch main with one initial commit
// containing base.txt, and returns its dir.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run(t, dir, "git", "init", "-b", "main")
	run(t, dir, "git", "config", "user.email", "test@example.com")
	run(t, dir, "git", "config", "user.name", "Test")
	run(t, dir, "git", "config", "commit.gpgsign", "false")
	writeFile(t, dir, "base.txt", "line1\nline2\nline3\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "initial")
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeBranch creates branch off the current HEAD, applies fn (which writes
// files), commits, and returns to the originalBranch.
func makeBranch(t *testing.T, dir, branch, originalBranch string, fn func()) {
	t.Helper()
	run(t, dir, "git", "checkout", "-b", branch)
	fn()
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "work on "+branch)
	run(t, dir, "git", "checkout", originalBranch)
}

func headOID(t *testing.T, dir string) string {
	t.Helper()
	return run(t, dir, "git", "rev-parse", "HEAD")
}

func TestConverge(t *testing.T) {
	t.Run("converged", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-feat", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/s-3-feat", TargetBranch: "main", RepoDir: dir})
		if res.Status != StatusConverged {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if res.Merge == "" {
			t.Fatalf("expected non-empty merge OID")
		}
		if !f.recorded("lease.release") {
			t.Fatalf("lease.release was not called; calls=%v", f.calls)
		}
		// main now contains the boundary's file.
		if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
			t.Fatalf("new.txt not on main after merge: %v", err)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		dir := initRepo(t)
		// Boundary edits base.txt line1.
		makeBranch(t, dir, "nm/s-3-conflict", "main", func() {
			writeFile(t, dir, "base.txt", "BRANCH\nline2\nline3\n")
		})
		// main edits the same line differently.
		writeFile(t, dir, "base.txt", "MAIN\nline2\nline3\n")
		run(t, dir, "git", "add", "-A")
		run(t, dir, "git", "commit", "-m", "main edits line1")
		before := headOID(t, dir)

		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/s-3-conflict", TargetBranch: "main", RepoDir: dir})
		if res.Status != StatusConflict {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if porc := run(t, dir, "git", "status", "--porcelain"); porc != "" {
			t.Fatalf("repo not clean after conflict abort: %q", porc)
		}
		if after := headOID(t, dir); after != before {
			t.Fatalf("main HEAD moved: before=%s after=%s", before, after)
		}
	})

	t.Run("gate_failed", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-gatefail", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		before := headOID(t, dir)
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/s-3-gatefail", TargetBranch: "main", RepoDir: dir, BoundaryDir: dir, Gate: "exit 1"})
		if res.Status != StatusGateFailed {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if after := headOID(t, dir); after != before {
			t.Fatalf("main HEAD moved despite gate failure: before=%s after=%s", before, after)
		}
		// The gate now runs BEFORE lease acquisition, so a gate failure must
		// hold no lease — proof the lease wraps only the merge.
		if f.recorded("lease.acquire") {
			t.Fatalf("gate failure must not acquire the trunk lease; calls=%v", f.calls)
		}
	})

	t.Run("dirty_tree_skips", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-dirty", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		before := headOID(t, dir)
		// Uncommitted modification to a TRACKED file in the launch checkout.
		writeFile(t, dir, "base.txt", "DIRTY\nline2\nline3\n")
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/s-3-dirty", TargetBranch: "main", RepoDir: dir})
		if res.Status != StatusSkipped {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if !strings.Contains(res.Reason, "uncommitted") || !strings.Contains(res.Reason, "tracked") {
			t.Fatalf("expected uncommitted/tracked reason, got %q", res.Reason)
		}
		if after := headOID(t, dir); after != before {
			t.Fatalf("main HEAD moved despite dirty tree: before=%s after=%s", before, after)
		}
	})

	t.Run("untracked_does_not_block", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-untracked", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		// Only an UNTRACKED file present (e.g. a repo's own AGENTS.md) — must
		// not block convergence.
		writeFile(t, dir, "AGENTS.md", "untracked\n")
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/s-3-untracked", TargetBranch: "main", RepoDir: dir})
		if res.Status != StatusConverged {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
	})

	t.Run("gate_passes_then_merges", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-gateok", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/s-3-gateok", TargetBranch: "main", RepoDir: dir, BoundaryDir: dir, Gate: "true"})
		if res.Status != StatusConverged {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
	})

	t.Run("skipped_drift", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-drift", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		// Move RepoDir HEAD to a different branch than TargetBranch.
		run(t, dir, "git", "checkout", "-b", "other")
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/s-3-drift", TargetBranch: "main", RepoDir: dir})
		if res.Status != StatusSkipped {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if !strings.Contains(res.Reason, "drift") {
			t.Fatalf("expected drift reason, got %q", res.Reason)
		}
	})

	t.Run("skipped_no_commits", func(t *testing.T) {
		dir := initRepo(t)
		// Boundary branch points at the same commit as main (no new commits).
		run(t, dir, "git", "branch", "nm/s-3-empty", "main")
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/s-3-empty", TargetBranch: "main", RepoDir: dir})
		if res.Status != StatusSkipped {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if !strings.Contains(res.Reason, "no new commits") {
			t.Fatalf("expected no-new-commits reason, got %q", res.Reason)
		}
	})

	t.Run("error_unsafe_name", func(t *testing.T) {
		dir := initRepo(t)
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "-rf", TargetBranch: "main", RepoDir: dir})
		if res.Status != StatusError {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if !strings.Contains(res.Reason, "unsafe branch name") {
			t.Fatalf("expected unsafe-name reason, got %q", res.Reason)
		}
	})

	// CONTAINER grain: the boundary branch lives only in a self-contained clone, so
	// Converge must FETCH it (Spec.From) into the canonical repo before merging. A
	// plain local `git clone` stands in for a container clone (no runtime needed).
	t.Run("from_fetch_converges", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not on PATH")
		}
		canonical := initRepo(t) // canonical repo, on main
		cloneParent := t.TempDir()
		clone := filepath.Join(cloneParent, "clone")
		// --no-hardlinks so the clone is a genuinely separate object store.
		run(t, cloneParent, "git", "clone", "--no-hardlinks", canonical, clone)
		run(t, clone, "git", "config", "user.email", "test@example.com")
		run(t, clone, "git", "config", "user.name", "Test")
		run(t, clone, "git", "config", "commit.gpgsign", "false")
		// Commit a new file on branch nm/x ONLY in the clone — it is NOT yet in the
		// canonical object store.
		makeBranch(t, clone, "nm/x", "main", func() {
			writeFile(t, clone, "fromclone.txt", "via fetch\n")
		})
		// The branch must not exist in the canonical repo before Converge.
		if _, err := os.Stat(filepath.Join(canonical, "fromclone.txt")); err == nil {
			t.Fatal("precondition: fromclone.txt already on canonical main")
		}

		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/x", TargetBranch: "main", RepoDir: canonical, From: clone, Gate: ""})
		if res.Status != StatusConverged {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		// Proof of fetch-then-merge: the clone-only file landed on canonical main.
		if _, err := os.Stat(filepath.Join(canonical, "fromclone.txt")); err != nil {
			t.Fatalf("fromclone.txt not on canonical main after From-fetch+merge: %v", err)
		}
		if !f.recorded("lease.release") {
			t.Fatalf("lease.release not called; calls=%v", f.calls)
		}
	})

	t.Run("from_bogus_path_errors", func(t *testing.T) {
		dir := initRepo(t)
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/x", TargetBranch: "main", RepoDir: dir, From: filepath.Join(dir, "does-not-exist")})
		if res.Status != StatusError {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if !strings.Contains(res.Reason, "container clone path not found") {
			t.Fatalf("expected clone-not-found reason, got %q", res.Reason)
		}
	})

	t.Run("from_flag_shaped_errors", func(t *testing.T) {
		dir := initRepo(t)
		f := &fakeRPC{}
		res := Converge(f, Spec{Branch: "nm/x", TargetBranch: "main", RepoDir: dir, From: "--upload-pack=evil"})
		if res.Status != StatusError {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if !strings.Contains(res.Reason, "unsafe --from") {
			t.Fatalf("expected unsafe-from reason, got %q", res.Reason)
		}
	})

	t.Run("lease_retry_then_release", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-retry", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		f := &fakeRPC{denyAcquires: 2}
		res := Converge(f, Spec{Branch: "nm/s-3-retry", TargetBranch: "main", RepoDir: dir})
		if res.Status != StatusConverged {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if !f.recorded("lease.release") {
			t.Fatalf("lease.release not called after retry; calls=%v", f.calls)
		}
	})

	// GATE SEAM: an injected GateRunner (the container grain's path) is the runner
	// the gate executes THROUGH — NOT the host `sh -c` default. This run proves the
	// runner fires (records the exact gate string) and that exit 0 converges.
	t.Run("gate_runner_used_and_passes", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-runner-ok", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		var gotGate string
		var calls int
		runner := func(gate string) (int, []byte, error) {
			calls++
			gotGate = gate
			return 0, []byte("runner-output"), nil
		}
		f := &fakeRPC{}
		res := Converge(f, Spec{
			Branch: "nm/s-3-runner-ok", TargetBranch: "main", RepoDir: dir,
			BoundaryDir: dir, Gate: "make conform", GateRunner: runner,
		})
		if res.Status != StatusConverged {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if calls != 1 {
			t.Fatalf("GateRunner ran %d times, want exactly 1 (the gate must execute THROUGH the seam)", calls)
		}
		if gotGate != "make conform" {
			t.Fatalf("GateRunner saw gate %q, want %q (the gate string must be passed as a single arg)", gotGate, "make conform")
		}
	})

	// NON-VACUOUS CONTROL for the run above: an injected GateRunner returning a
	// NON-ZERO exit (with NIL error — proving the `code != 0` branch, not just the
	// err branch, flips the status) ⇒ StatusGateFailed, NOT merged, and NO trunk
	// lease acquired (the gate runs before lease acquisition).
	t.Run("gate_runner_nonzero_fails_no_merge", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-runner-fail", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		before := headOID(t, dir)
		runner := func(gate string) (int, []byte, error) {
			return 7, []byte("boom"), nil // non-zero exit, NIL error
		}
		f := &fakeRPC{}
		res := Converge(f, Spec{
			Branch: "nm/s-3-runner-fail", TargetBranch: "main", RepoDir: dir,
			BoundaryDir: dir, Gate: "exit 7", GateRunner: runner,
		})
		if res.Status != StatusGateFailed {
			t.Fatalf("status=%q reason=%q, want gate_failed", res.Status, res.Reason)
		}
		if !strings.Contains(res.Reason, "exit status 7") {
			t.Fatalf("expected the non-zero exit code in the reason, got %q", res.Reason)
		}
		if after := headOID(t, dir); after != before {
			t.Fatalf("main HEAD moved despite gate failure: before=%s after=%s", before, after)
		}
		if f.recorded("lease.acquire") {
			t.Fatalf("a failed gate must not acquire the trunk lease; calls=%v", f.calls)
		}
	})

	// GateRunner nil ⇒ the HOST `sh -c` default runs in BoundaryDir (unchanged). A
	// gate that writes a sentinel file proves the host path actually executed when no
	// runner is injected (the worktree grain's behavior is byte-identical).
	t.Run("nil_runner_uses_host_default", func(t *testing.T) {
		dir := initRepo(t)
		makeBranch(t, dir, "nm/s-3-hostgate", "main", func() {
			writeFile(t, dir, "new.txt", "hello\n")
		})
		boundary := t.TempDir()
		sentinel := filepath.Join(boundary, "gate-ran")
		f := &fakeRPC{}
		res := Converge(f, Spec{
			Branch: "nm/s-3-hostgate", TargetBranch: "main", RepoDir: dir,
			BoundaryDir: boundary, Gate: "touch gate-ran", // relative ⇒ proves cwd==BoundaryDir
		})
		if res.Status != StatusConverged {
			t.Fatalf("status=%q reason=%q", res.Status, res.Reason)
		}
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("host default gate did not run in BoundaryDir (no sentinel): %v", err)
		}
	})
}
