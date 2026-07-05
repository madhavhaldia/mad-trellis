package main

// START-IF-ABSENT tests (chafe C12): the auto-start path must restore the ambient
// UX (a governed launch with no daemon present auto-starts one and proceeds) while
// staying FAIL-CLOSED (if the daemon cannot be made present, BLOCK exit 126 —
// the agent never runs). Hermetic: scratch sockets + scratch runtime dirs only.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/daemon"
)

var (
	builtBinOnce sync.Once
	builtBinPath string
	builtBinErr  error
)

// buildBinary compiles the mad-trellis CLI to a temp path once per test binary and
// returns its path. cgo-free, matching the production build constraint.
func buildBinary(t *testing.T) string {
	t.Helper()
	builtBinOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "nmbin-*")
		if err != nil {
			builtBinErr = err
			return
		}
		out := filepath.Join(dir, "mad-trellis")
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			builtBinErr = err
			builtBinPath = string(b)
			return
		}
		builtBinPath = out
	})
	if builtBinErr != nil {
		t.Fatalf("build mad-trellis binary: %v\n%s", builtBinErr, builtBinPath)
	}
	return builtBinPath
}

// ensureDaemon with NO daemon present auto-starts one and returns nil; the daemon
// is then reachable. Proves the ambient-UX path.
func TestEnsureDaemonAutoStarts(t *testing.T) {
	bin := buildBinary(t)
	sock := scratchSock(t)
	if running, _ := daemon.IsRunning(sock); running {
		t.Fatal("precondition: no daemon should be running")
	}
	err := ensureDaemon(sock, bin, func(string, ...any) {})
	if err != nil {
		t.Fatalf("ensureDaemon must auto-start a daemon, got: %v", err)
	}
	if !daemonReady(sock) {
		t.Fatal("socket must accept after auto-start")
	}
	// Clean up the auto-started daemon: stop it via the pidfile.
	t.Cleanup(func() {
		if pid, present, _ := daemon.ReadPidfile(daemon.PidfilePath(sock)); present {
			if p, _ := os.FindProcess(pid); p != nil {
				_ = p.Signal(os.Interrupt)
				_ = p.Kill()
			}
		}
	})
	if running, _ := daemon.IsRunning(sock); !running {
		t.Fatal("daemon must hold the flock after auto-start")
	}
}

// ensureDaemon is a no-op when a daemon is already running (does not spawn a
// second one — the flock would dedupe, but we should not even try).
func TestEnsureDaemonNoOpWhenRunning(t *testing.T) {
	sock := scratchSock(t)
	startInProcDaemon(t, sock)
	// selfBin deliberately bogus: if ensureDaemon tried to spawn, it would error.
	if err := ensureDaemon(sock, "/nonexistent/bogus/binary", func(string, ...any) {}); err != nil {
		t.Fatalf("ensureDaemon must be a no-op when a daemon already runs, got: %v", err)
	}
}

// FAIL-CLOSED: a bogus self-binary path cannot spawn a daemon → ensureDaemon
// returns an error (the caller BLOCKs). The socket never becomes ready.
func TestEnsureDaemonFailClosedBogusBinary(t *testing.T) {
	sock := scratchSock(t)
	err := ensureDaemon(sock, "/nonexistent/definitely/not/a/binary", func(string, ...any) {})
	if err == nil {
		t.Fatal("ensureDaemon must FAIL (BLOCK) when the daemon cannot be auto-started")
	}
	if daemonReady(sock) {
		t.Fatal("no daemon should be reachable after a failed auto-start")
	}
}

// FAIL-CLOSED: an empty self-binary path (os.Executable() failure) → error.
func TestEnsureDaemonFailClosedUnknownSelf(t *testing.T) {
	sock := scratchSock(t)
	if err := ensureDaemon(sock, "", func(string, ...any) {}); err == nil {
		t.Fatal("ensureDaemon must FAIL when the self binary is unknown")
	}
}

// END-TO-END governed launch with start-if-absent: invoke the built binary as
// `mad-trellis launch -- <fake-agent>` with NO daemon running. It must auto-start a
// daemon and then run the fake agent (governed), exiting with the agent's own
// code — NOT BlockedExitCode.
func TestLaunchAutoStartsDaemonAndRunsAgentEndToEnd(t *testing.T) {
	bin := buildBinary(t)
	sock := scratchSock(t)
	runtimeDir := t.TempDir()
	repo := initGitRepo(t) // the substrate provisions a worktree off a real git repo

	// A fake "agent": /bin/echo exits 0 after printing. The launcher execs it on a
	// PTY inside the governed boundary.
	fakeAgent := "/bin/echo"
	if _, err := os.Stat(fakeAgent); err != nil {
		t.Skipf("no %s on this host", fakeAgent)
	}

	cmd := exec.Command(bin, "launch", "--socket", sock, "--", fakeAgent, "governed-hello")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"MAD_RUNTIME_DIR="+runtimeDir,
		"MAD_WORKTREE_DIR="+t.TempDir(), // isolate the provisioned worktree
		"MAD_STATE_DIR="+t.TempDir(),
		"MAD_DEBUG=1")
	out, err := combinedWithTimeout(t, cmd, 30*time.Second)

	t.Cleanup(func() {
		if pid, present, _ := daemon.ReadPidfile(daemon.PidfilePath(sock)); present {
			if p, _ := os.FindProcess(pid); p != nil {
				_ = p.Kill()
			}
		}
	})

	if err != nil {
		// A non-zero exit here would be the BLOCK (126) or an agent failure.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 126 {
			t.Fatalf("launch BLOCKED (126) instead of auto-starting; out:\n%s", out)
		}
		t.Fatalf("launch failed: %v; out:\n%s", err, out)
	}
	if !strings.Contains(out, "auto-starting") {
		t.Fatalf("expected the 'auto-starting' log line; out:\n%s", out)
	}
	if !strings.Contains(out, "governed-hello") {
		t.Fatalf("the fake agent must have run (its output is missing); out:\n%s", out)
	}
	// A daemon must now be running (the auto-started one persists).
	if running, _ := daemon.IsRunning(sock); !running {
		t.Fatal("the auto-started daemon must still be running after the agent exits")
	}
}

// END-TO-END fail-closed: governed launch with auto-start IMPOSSIBLE (the runtime
// dir's derived ledger location is unwritable) → BLOCK exit 126, the agent never
// runs. We make auto-start impossible by pointing the socket into a directory
// that does not exist and cannot be created as a child of a regular file.
func TestLaunchFailsClosedWhenAutoStartImpossibleEndToEnd(t *testing.T) {
	bin := buildBinary(t)

	// Create a regular FILE, then put the socket "under" it as if it were a dir:
	// <file>/daemon.sock. The daemon child cannot bind there (ENOTDIR), so the
	// socket never becomes ready → ensureDaemon times out → BLOCK.
	base := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(base, "daemon.sock") // unbindable: parent is a file

	fakeAgent := "/bin/echo"
	if _, err := os.Stat(fakeAgent); err != nil {
		t.Skipf("no %s on this host", fakeAgent)
	}

	cmd := exec.Command(bin, "launch", "--socket", sock, "--", fakeAgent, "SHOULD-NOT-RUN")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "MAD_DEBUG=1")
	out, err := combinedWithTimeout(t, cmd, 30*time.Second)

	if err == nil {
		t.Fatalf("launch must BLOCK when auto-start is impossible; out:\n%s", out)
	}
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 126 {
		t.Fatalf("expected BLOCK exit 126, got %v; out:\n%s", err, out)
	}
	if strings.Contains(out, "SHOULD-NOT-RUN") {
		t.Fatalf("FAIL-CLOSED VIOLATED: the agent ran despite a blocked launch; out:\n%s", out)
	}
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("expected a BLOCKED diagnostic; out:\n%s", out)
	}
}

// initGitRepo creates a minimal git repo with one commit (the substrate
// provisions a worktree off it). Mirrors internal/substrate's test setup.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

// combinedWithTimeout runs cmd capturing combined output, killing it if it
// overruns timeout (there is no `timeout` binary on the macOS host).
func combinedWithTimeout(t *testing.T, cmd *exec.Cmd, timeout time.Duration) (string, error) {
	t.Helper()
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("command timed out after %s; partial out:\n%s", timeout, buf.String())
		return buf.String(), nil
	}
}
