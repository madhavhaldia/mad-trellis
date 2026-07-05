// Package conductor implements the deterministic (NO LLM) automatic-convergence
// engine: given a finished agent's boundary branch, it acquires the single trunk
// lease from the daemon (to serialize merges), optionally runs a shell gate
// command, then merges the boundary branch into a target branch in-repo with
// `git merge --no-ff`.
//
// A conflict or gate failure leaves the target branch byte-identical and returns
// a structured status (a future "Wing 3" escalates those). The package is
// cgo-free and imports only the Go stdlib (plus os/exec for git) — no other
// internal packages, no third-party deps.
package conductor

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// RPCClient is the minimal daemon-RPC surface the conductor needs.
// The launcher's session satisfies this.
type RPCClient interface {
	Call(method string, params any, result any) error
}

// Spec describes one convergence of a finished boundary.
type Spec struct {
	Branch       string                           // boundary branch to converge, e.g. "nm/s-3-abc"
	TargetBranch string                           // branch to merge onto (captured at launch), e.g. "feat/x"
	RepoDir      string                           // canonical repo working dir where the merge runs (the launch cwd)
	BoundaryDir  string                           // boundary worktree dir where the gate runs (deps installed there)
	Gate         string                           // shell gate command; "" => merge-only (no gate)
	LeaseTTLMs   int                              // trunk-lease TTL; 0 => default (120000 = 2 min)
	Logf         func(format string, args ...any) // optional progress log; nil => silent

	// From, when non-empty, is a CONTAINER-grain clone path: Converge first fetches
	// <Branch> from it into RepoDir (git fetch --no-tags <From> +<Branch>:<Branch>) so
	// the branch exists in the canonical object store before the merge. Empty for the
	// worktree grain (the branch is already in the shared .git).
	From string

	// GateRunner, when non-nil, runs the shell Gate and returns its exit code,
	// captured output, and any operational error. It is the grain seam: the
	// CONTAINER grain injects a runner that executes the gate INSIDE the boundary
	// container (via `container exec`), where the agent's deps actually live, rather
	// than on the host clone. When nil, the gate falls back to the host default
	// (`sh -c <Gate>` in BoundaryDir) — BYTE-IDENTICAL to the pre-seam behavior.
	// Gate semantics are unchanged for either runner: a non-zero exit (or a runner
	// error) ⇒ StatusGateFailed, NOT merged, and (since the gate runs before lease
	// acquisition) no lease is leaked.
	GateRunner func(gate string) (exitCode int, output []byte, err error)
}

// Status is the structured outcome of a convergence.
type Status string

const (
	StatusConverged  Status = "converged"   // merged onto target
	StatusGateFailed Status = "gate_failed" // gate command exited non-zero; NOT merged (Wing 3 escalates)
	StatusConflict   Status = "conflict"    // merge conflict, aborted cleanly (Wing 3 escalates)
	StatusSkipped    Status = "skipped"     // nothing to do (target drifted, or no new commits)
	StatusError      Status = "error"       // operational failure (lease, bad input, git error)
)

// Result is the encoded outcome of Converge.
type Result struct {
	Status Status
	Reason string // human-readable detail; empty on clean StatusConverged
	Merge  string // merge commit OID on StatusConverged; empty otherwise
}

const defaultLeaseTTLMs = 120000 // 2 minutes — the lease now wraps only the fast merge

// Converge runs the deterministic convergence for one finished boundary.
// It NEVER returns an error — every outcome is encoded in Result.Status so the
// caller (the launcher, fail-soft) can log and move on. It always releases the
// trunk lease it acquired, on every path.
//
// Order of operations: the read-only guards (drift / nothing-to-converge /
// dirty-tree) and the gate all run WITHOUT the trunk lease held — the gate
// validates the BOUNDARY in isolation and does not need the trunk. The lease is
// acquired only around the fast merge, so a long gate no longer blocks all
// convergence and a gate failure holds no lease at all.
func Converge(cl RPCClient, spec Spec) Result {
	logf := spec.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// 1. Validate branch names so neither can be parsed by git as a FLAG
	// (arg-injection): both are passed positionally to git.
	if !safeBranchName(spec.Branch) {
		return Result{Status: StatusError, Reason: "unsafe branch name " + quote(spec.Branch)}
	}
	if !safeBranchName(spec.TargetBranch) {
		return Result{Status: StatusError, Reason: "unsafe branch name " + quote(spec.TargetBranch)}
	}

	// 1b. CONTAINER grain: the boundary branch lives only in the agent's
	// self-contained clone, not the canonical object store. Fetch it into RepoDir
	// (mirrors `integrate --from`) so the drift/nothing-to-converge/merge steps
	// below — all of which need the branch present locally — work as for a worktree.
	if spec.From != "" {
		// arg-injection guard: From is a positional `git fetch` remote.
		if strings.HasPrefix(spec.From, "-") {
			return Result{Status: StatusError, Reason: "unsafe --from " + quote(spec.From)}
		}
		if fi, e := os.Stat(spec.From); e != nil || !fi.IsDir() {
			return Result{Status: StatusError, Reason: "container clone path not found: " + spec.From}
		}
		if out, e := git(spec.RepoDir, "fetch", "--no-tags", spec.From, "+"+spec.Branch+":"+spec.Branch); e != nil {
			return Result{Status: StatusError, Reason: "fetch container branch: " + out}
		}
	}

	// 2. Target-drift guard (read-only, NO lease): only auto-merge onto the
	// branch we forked from.
	head, err := git(spec.RepoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Result{Status: StatusError, Reason: "rev-parse HEAD: " + head}
	}
	if head != spec.TargetBranch {
		return Result{Status: StatusSkipped, Reason: "target branch drifted (HEAD=" + head + ", expected " + spec.TargetBranch + "); run `mad-trellis integrate` manually"}
	}

	// 3. Nothing-to-converge guard (read-only, NO lease).
	count, err := git(spec.RepoDir, "rev-list", "--count", spec.TargetBranch+".."+spec.Branch)
	if err != nil {
		return Result{Status: StatusError, Reason: "rev-list count: " + count}
	}
	if count == "0" {
		return Result{Status: StatusSkipped, Reason: "no new commits on " + spec.Branch}
	}

	// 4. Dirty-tree guard (read-only, NO lease): a `git merge` against a launch
	// checkout with uncommitted changes to TRACKED files would refuse (and the
	// conductor would mislabel it a conflict). Skip cleanly instead. Untracked
	// files (e.g. a repo's own AGENTS.md/CLAUDE.md) are intentionally ignored
	// via --untracked-files=no, so the conductor still converges on such repos.
	dirty, err := git(spec.RepoDir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return Result{Status: StatusError, Reason: "status --porcelain: " + dirty}
	}
	if strings.TrimSpace(dirty) != "" {
		return Result{Status: StatusSkipped, Reason: spec.TargetBranch + " has uncommitted changes to tracked files; commit or stash, then run `mad-trellis integrate " + spec.Branch + "`"}
	}

	// 5. Run the gate (only if configured), with NO lease held — it validates
	// the boundary in isolation. The runner is the grain seam: a CONTAINER grain
	// injects spec.GateRunner to run the gate INSIDE the boundary container; a nil
	// runner falls back to the host default (`sh -c` in BoundaryDir), byte-identical
	// to the pre-seam behavior. A non-zero exit or a runner error ⇒ StatusGateFailed
	// (NOT merged); the gate runs before lease acquisition, so no lease is leaked.
	if spec.Gate != "" {
		logf("running gate: %s", spec.Gate)
		runner := spec.GateRunner
		if runner == nil {
			runner = hostGateRunner(spec.BoundaryDir)
		}
		code, _, gErr := runner(spec.Gate)
		if gErr != nil {
			return Result{Status: StatusGateFailed, Reason: "gate command failed: " + gErr.Error()}
		}
		if code != 0 {
			return Result{Status: StatusGateFailed, Reason: "gate command failed: exit status " + strconv.Itoa(code)}
		}
		logf("gate passed")
	}

	// 6. Resolve the trunk lease key from the classifier (Route(trunk)).
	var route struct {
		LeaseKey string `json:"lease_key"`
	}
	if err := cl.Call("classify.route", map[string]string{"domain": "trunk"}, &route); err != nil {
		return Result{Status: StatusError, Reason: "classify.route: " + err.Error()}
	}
	if route.LeaseKey == "" {
		return Result{Status: StatusError, Reason: "daemon returned no trunk lease key"}
	}

	ttl := spec.LeaseTTLMs
	if ttl == 0 {
		ttl = defaultLeaseTTLMs
	}

	// 7. Acquire the trunk lease; retry briefly if another convergence holds it.
	// The lease now wraps ONLY the fast merge below — well within the TTL — so
	// there is no background renew.
	acquired := false
	for i := 0; i < 120; i++ {
		var acq struct {
			Granted bool   `json:"granted"`
			Holder  string `json:"holder"`
		}
		if err := cl.Call("lease.acquire", map[string]any{"key": route.LeaseKey, "ttl_ms": ttl}, &acq); err != nil {
			return Result{Status: StatusError, Reason: "lease.acquire: " + err.Error()}
		}
		if acq.Granted {
			acquired = true
			break
		}
		if i == 0 {
			logf("trunk busy (held by %s) — waiting", acq.Holder)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !acquired {
		return Result{Status: StatusError, Reason: "trunk lease busy"}
	}
	defer cl.Call("lease.release", map[string]any{"key": route.LeaseKey}, nil)

	// 8. Re-check nothing-to-converge: another conductor may have merged this
	// branch while the (unlocked) gate ran. Drift/dirty are not re-checked —
	// unlikely to change mid-flow, and the merge itself re-validates conflicts.
	count, err = git(spec.RepoDir, "rev-list", "--count", spec.TargetBranch+".."+spec.Branch)
	if err != nil {
		return Result{Status: StatusError, Reason: "rev-list count: " + count}
	}
	if count == "0" {
		return Result{Status: StatusSkipped, Reason: "already converged / no new commits on " + spec.Branch}
	}

	// 9. Merge. A conflict aborts cleanly so the target is left byte-identical.
	if out, mErr := git(spec.RepoDir, "merge", "--no-ff", "--no-edit", spec.Branch); mErr != nil {
		_, _ = git(spec.RepoDir, "merge", "--abort")
		return Result{Status: StatusConflict, Reason: "merge conflict (aborted): " + out}
	}
	oid, err := git(spec.RepoDir, "rev-parse", "HEAD")
	if err != nil {
		return Result{Status: StatusError, Reason: "rev-parse merge HEAD: " + oid}
	}
	logf("converged %s onto %s (%s)", spec.Branch, spec.TargetBranch, oid)
	return Result{Status: StatusConverged, Merge: oid}
}

// safeBranchName reports whether a branch/ref name is safe to pass to git as a
// POSITIONAL argument: non-empty, NOT starting with '-' (so git can never parse
// it as a flag), and only [A-Za-z0-9._/-] (covers nm/<slug>). This mirrors the
// `integrate` command's guard and closes git arg-injection.
func safeBranchName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '/' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// hostGateRunner is the DEFAULT GateRunner (used when Spec.GateRunner is nil): it
// runs the gate as `sh -c <gate>` in boundaryDir with output streamed live to the
// launcher's stdout/stderr — BYTE-IDENTICAL to the pre-seam host gate. The exit
// code is resolved from the process state; a non-zero exit surfaces as a non-nil
// *exec.ExitError (and a non-zero code), which the caller maps to StatusGateFailed.
func hostGateRunner(boundaryDir string) func(gate string) (int, []byte, error) {
	return func(gate string) (int, []byte, error) {
		gc := exec.Command("sh", "-c", gate)
		gc.Dir = boundaryDir
		gc.Stdout = os.Stdout
		gc.Stderr = os.Stderr
		err := gc.Run()
		return exitCodeOf(err), nil, err
	}
}

// exitCodeOf resolves a process exit code from an os/exec run error: 0 on nil,
// the real code on an *exec.ExitError, and -1 for a non-exit operational failure
// (e.g. the binary could not start).
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// git runs `git -C dir args...` and returns the trimmed combined output.
func git(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func quote(s string) string {
	return "\"" + s + "\""
}
