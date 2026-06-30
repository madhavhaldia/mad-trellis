//go:build packaging

// smoke_test.go is the hermetic packaging smoke for project 10b: it builds the
// real binary and drives the FULL operator lifecycle end to end — version,
// doctor (daemon down), init, daemon up, status (running), stop, status (gone) —
// to prove a shipped binary actually works, not just compiles.
//
// HERMETICITY (load-bearing): this test NEVER touches the real ~/.mad-substrate and
// NEVER talks to a real daemon. Everything it exercises is either a freshly built
// binary in t.TempDir() or a scratch path it created and tears down:
//   - the runtime dir is a scratch MAD_RUNTIME_DIR (also pinned via
//     MAD_HOME / MAD_WORKTREE_DIR / MAD_STATE_DIR so no resolver
//     can fall back to $HOME), under t.TempDir();
//   - the socket is an explicit --socket in a short /tmp dir (the unix-socket path
//     limit is ~104 bytes on darwin / ~108 on linux; t.TempDir() under TMPDIR can
//     blow that — /tmp keeps us safely under the stricter bound on either OS),
//     removed at the end;
//   - git is neutered with GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_SYSTEM=
//     /dev/null so the user's real git config never leaks in.
//
// The only daemon ever started is the scratch one bound to the scratch socket,
// and it is killed in teardown. No real daemon is referenced or signalled.
package packaging

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hermeticEnv returns the environment every mad-substrate/git invocation in the smoke
// test runs under: a scratch runtime dir pinned through ALL the runtime-dir env
// vars (so no resolver can reach $HOME), an explicit socket, and neutered git
// config. It starts from os.Environ() so PATH/toolchain stay intact, then
// overrides the governance-relevant vars.
func hermeticEnv(runtimeDir, socket string) []string {
	env := os.Environ()
	overrides := map[string]string{
		// Pin the runtime dir everywhere a resolver might look (Go reads
		// _RUNTIME_DIR then _HOME; pin both, plus the worktree/state dirs, so a
		// fallback to the user's real ~/.mad-substrate is impossible).
		"MAD_RUNTIME_DIR":  runtimeDir,
		"MAD_HOME":         runtimeDir,
		"MAD_WORKTREE_DIR": filepath.Join(runtimeDir, "worktrees"),
		"MAD_STATE_DIR":    filepath.Join(runtimeDir, "state"),
		"MAD_SOCKET":       socket,
		// Neuter git config so the real user/system config never influences the
		// scratch repo or the daemon's git invocations.
		"GIT_CONFIG_GLOBAL": "/dev/null",
		"GIT_CONFIG_SYSTEM": "/dev/null",
	}
	// Replace any pre-existing occurrences, then append the rest.
	out := env[:0:0]
	seen := map[string]bool{}
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		key := kv[:eq]
		if v, ok := overrides[key]; ok {
			out = append(out, key+"="+v)
			seen[key] = true
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// runGit runs a git command in dir under the neutered/hermetic env, failing the
// test on error.
func runGit(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (in %s) failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// nm runs the built mad-substrate binary with args, cwd, and the hermetic env, and
// returns combined output + exit code (-1 if it failed to start).
func nm(t *testing.T, bin, dir string, env, args []string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("could not run %s %s: %v", bin, strings.Join(args, " "), err)
		}
	}
	return string(out), code
}

// dialReady polls net.Dial("unix", sock) until it connects or the deadline
// passes; returns true once a connection succeeds.
func dialReady(sock string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", sock, 100*time.Millisecond); err == nil {
			c.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// dialRefused polls until net.Dial("unix", sock) is REFUSED (the daemon is gone)
// or the deadline passes; returns true once the socket stops accepting.
func dialRefused(sock string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", sock, 100*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestSmokeCleanTarget exercises the full operator lifecycle against a freshly
// built binary on entirely scratch paths (see the package/file hermeticity note).
func TestSmokeCleanTarget(t *testing.T) {
	root := repoRoot(t)

	// --- build the binary, cgo-free, into a scratch dir ---------------------
	bin := filepath.Join(t.TempDir(), "mad-substrate")
	build := exec.Command("go", "build", "-o", bin, "./cmd/mad-substrate")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 build of ./cmd/mad-substrate failed: %v\n%s", err, out)
	}

	// --- scratch runtime dir + a SHORT /tmp socket --------------------------
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	// Unix-socket paths are capped at ~104 bytes on darwin / ~108 on linux; a path
	// under TMPDIR/t.TempDir() can exceed that, so put the socket in a short /tmp
	// dir we own and remove (/tmp exists and is short on both OSes).
	sockDir, err := os.MkdirTemp("/tmp", "nmpkg-")
	if err != nil {
		t.Fatalf("mkdtemp /tmp socket dir: %v", err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "d.sock")
	// Assert against the STRICTER (darwin) bound so the test stays valid on either
	// OS — a path under 104 bytes is safe on linux too.
	if len(sock) > 104 {
		t.Fatalf("socket path %q exceeds the 104-byte unix-socket limit (%d bytes)", sock, len(sock))
	}

	env := hermeticEnv(runtimeDir, sock)

	// --- version -> exit 0, mentions mad-substrate ------------------------------
	out, code := nm(t, bin, root, env, []string{"version"})
	if code != 0 {
		t.Fatalf("`version` exit %d; want 0\n%s", code, out)
	}
	if !strings.Contains(out, "mad-substrate") {
		t.Fatalf("`version` output must contain \"mad-substrate\"; got %q", out)
	}

	// --- a scratch governed git repo (the daemon cwd) -----------------------
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit(t, repo, env, "init", "-b", "main")
	// A local identity so the seed commit succeeds under the neutered git config.
	runGit(t, repo, env, "config", "user.email", "smoke@mad-substrate.test")
	runGit(t, repo, env, "config", "user.name", "mad-substrate smoke")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# scratch\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, repo, env, "add", "README.md")
	runGit(t, repo, env, "commit", "-m", "seed")

	// --- doctor BEFORE the daemon -> exit 0 (git present; daemon-down is warn) -
	out, code = nm(t, bin, repo, env, []string{"doctor", "--socket", sock})
	if code != 0 {
		t.Fatalf("`doctor` (daemon down) exit %d; want 0 (git present, daemon-not-running is a warn)\n%s", code, out)
	}
	if !strings.Contains(out, sock) {
		t.Fatalf("`doctor` output must mention the resolved socket %q; got:\n%s", sock, out)
	}
	if !strings.Contains(strings.ToLower(out), "git") {
		t.Fatalf("`doctor` output must mention \"git\"; got:\n%s", out)
	}

	// --- init in a scratch dir -> writes mad-substrate.json ---------------------
	initDir := filepath.Join(t.TempDir(), "initrepo")
	if err := os.MkdirAll(initDir, 0o755); err != nil {
		t.Fatalf("mkdir init dir: %v", err)
	}
	out, code = nm(t, bin, initDir, env, []string{"init"})
	if code != 0 {
		t.Fatalf("`init` exit %d; want 0\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(initDir, "mad-substrate.json")); err != nil {
		t.Fatalf("`init` must write mad-substrate.json: %v\n%s", err, out)
	}

	// --- start the daemon in the background (cwd = scratch repo) ------------
	daemon := exec.Command(bin, "daemon", "--socket", sock)
	daemon.Dir = repo
	daemon.Env = env
	if err := daemon.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	// Teardown: ALWAYS kill the scratch daemon (only this scratch process is ever
	// signalled — never a real daemon).
	daemonKilled := false
	killDaemon := func() {
		if daemonKilled || daemon.Process == nil {
			return
		}
		_ = daemon.Process.Kill()
		_, _ = daemon.Process.Wait()
		daemonKilled = true
	}
	defer killDaemon()

	if !dialReady(sock, 10*time.Second) {
		t.Fatalf("daemon did not become reachable on %s within 10s", sock)
	}

	// The daemon initializes the ledger at <socket-dir>/ledger.db on first start.
	ledger := filepath.Join(filepath.Dir(sock), "ledger.db")
	if _, err := os.Stat(ledger); err != nil {
		t.Fatalf("daemon must create the ledger at %s on first start: %v", ledger, err)
	}

	// --- daemon status -> exit 0, "running" ---------------------------------
	out, code = nm(t, bin, repo, env, []string{"daemon", "status", "--socket", sock})
	if code != 0 {
		t.Fatalf("`daemon status` (running) exit %d; want 0\n%s", code, out)
	}
	if !strings.Contains(out, "running") {
		t.Fatalf("`daemon status` output must contain \"running\"; got %q", out)
	}

	// --- daemon stop -> exit 0, "stopped" -----------------------------------
	out, code = nm(t, bin, repo, env, []string{"daemon", "stop", "--socket", sock})
	if code != 0 {
		t.Fatalf("`daemon stop` exit %d; want 0\n%s", code, out)
	}
	if !strings.Contains(out, "stopped") {
		t.Fatalf("`daemon stop` output must contain \"stopped\"; got %q", out)
	}
	// The daemon exited cleanly via SIGTERM; reap it so the deferred kill is a
	// no-op and the socket is gone.
	_, _ = daemon.Process.Wait()
	daemonKilled = true
	if !dialRefused(sock, 5*time.Second) {
		t.Fatalf("socket %s still reachable 5s after `daemon stop`", sock)
	}

	// --- daemon status again -> exit 1, "not running" -----------------------
	out, code = nm(t, bin, repo, env, []string{"daemon", "status", "--socket", sock})
	if code != 1 {
		t.Fatalf("`daemon status` (after stop) exit %d; want 1\n%s", code, out)
	}
	if !strings.Contains(out, "not running") {
		t.Fatalf("`daemon status` (after stop) output must contain \"not running\"; got %q", out)
	}
}
