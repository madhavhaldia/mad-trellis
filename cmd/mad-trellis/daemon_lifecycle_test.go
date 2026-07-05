package main

// CLI lifecycle hardening tests (chafe C12 + C14). Hermetic: every daemon here
// runs on its OWN scratch socket under a scratch MAD_RUNTIME_DIR and is
// killed/closed by the test; nothing touches ~/.mad-trellis or a real daemon.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/app"
	"github.com/madhavhaldia/mad-trellis/internal/daemon"
)

// scratchSock returns a short /tmp socket path (Unix sun_path is ~104 bytes) with
// its lock/pid siblings cleaned up after the test.
func scratchSock(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "nmctl-*.sock")
	if err != nil {
		t.Fatalf("tmp socket: %v", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	t.Cleanup(func() {
		_ = os.Remove(name)
		_ = os.Remove(name + ".lock")
		_ = os.Remove(name + ".pid")
	})
	return name
}

// startInProcDaemon composes + starts a real daemon on sock (in this process) and
// serves it until the test ends. Returns the *daemon.Daemon so a test can Close
// it. Used by status/IsRunning assertions that don't need a separate pid.
func startInProcDaemon(t *testing.T, sock string) *daemon.Daemon {
	t.Helper()
	d, closeAll, err := app.Build(app.Config{
		SocketPath: sock,
		LedgerPath: filepath.Join(t.TempDir(), "ledger.db"),
		RepoRoot:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build daemon: %v", err)
	}
	if err := d.Start(); err != nil {
		closeAll()
		t.Fatalf("start daemon: %v", err)
	}
	go func() { _ = d.Serve() }()
	t.Cleanup(func() { _ = d.Close(); _ = closeAll() })
	// Wait for the socket to accept.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if daemonReady(sock) {
			return d
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("in-proc daemon did not become ready")
	return nil
}

// `daemon status` against a live daemon reports running + the pidfile pid.
func TestDaemonStatusRunningReportsPidfile(t *testing.T) {
	sock := scratchSock(t)
	startInProcDaemon(t, sock)

	cmd := daemonStatusCmd(&sock)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status against a live daemon must exit 0, got %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "running") {
		t.Fatalf("expected 'running', got %q", s)
	}
	// The reported pid must be this process's pid (the in-proc daemon wrote it).
	if !strings.Contains(s, "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("status should report the pidfile pid %d, got %q", os.Getpid(), s)
	}
}

// `daemon status` decides not-running from the FLOCK even if a STALE pidfile is
// present (a crash leftover).
func TestDaemonStatusStalePidfileNotRunning(t *testing.T) {
	sock := scratchSock(t)
	// No daemon; seed a stale pidfile. The flock is free → not running.
	if err := os.WriteFile(daemon.PidfilePath(sock), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := daemonStatusCmd(&sock)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	se, ok := err.(errSilentExit)
	if !ok || se.code == 0 {
		t.Fatalf("stale pidfile + free flock must report not-running (exit 1), got %T %v", err, err)
	}
	if !strings.Contains(out.String(), "not running") {
		t.Fatalf("expected 'not running', got %q", out.String())
	}
}

// `daemon stop` against a STALE pidfile (no live daemon) is a clean no-op (exit
// 0), reports "not running", and cleans the pidfile up — never an error.
func TestDaemonStopStalePidfileCleanNoOp(t *testing.T) {
	sock := scratchSock(t)
	if err := os.WriteFile(daemon.PidfilePath(sock), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := daemonStopCmd(&sock)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stop on a stale pidfile must be a clean no-op, got %v", err)
	}
	if !strings.Contains(out.String(), "not running") {
		t.Fatalf("expected 'not running', got %q", out.String())
	}
	if _, present, _ := daemon.ReadPidfile(daemon.PidfilePath(sock)); present {
		t.Fatal("stop must clean up the stale pidfile")
	}
}

// END-TO-END: build the binary, start a daemon child on a scratch socket, confirm
// the pidfile is written + names the CHILD's pid, then `daemon stop` kills that
// exact child via the pidfile and removes the pidfile. This is the C14 authority
// path (stop signals the pidfile's pid, not a self-report).
func TestDaemonStopKillsPidfileProcessEndToEnd(t *testing.T) {
	bin := buildBinary(t)
	sock := scratchSock(t)
	runtimeDir := t.TempDir()

	child := exec.Command(bin, "daemon", "--socket", sock)
	child.Dir = t.TempDir()
	child.Env = append(os.Environ(), "MAD_RUNTIME_DIR="+runtimeDir)
	if err := child.Start(); err != nil {
		t.Fatalf("start daemon child: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	}()

	// Wait for readiness + pidfile.
	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if daemonReady(sock) {
			if p, present, _ := daemon.ReadPidfile(daemon.PidfilePath(sock)); present {
				pid = p
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("daemon child did not become ready / write a pidfile in time")
	}
	if pid != child.Process.Pid {
		t.Fatalf("pidfile must record the daemon child's pid %d, got %d", child.Process.Pid, pid)
	}

	// Stop via the CLI command (uses the pidfile as authority).
	cmd := daemonStopCmd(&sock)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stop must succeed, got %v (out=%q)", err, out.String())
	}
	if !strings.Contains(out.String(), "stopped daemon") {
		t.Fatalf("expected 'stopped daemon', got %q", out.String())
	}
	// The child must actually be gone and the pidfile removed.
	if _, present, _ := daemon.ReadPidfile(daemon.PidfilePath(sock)); present {
		t.Fatal("pidfile must be removed after the daemon stops")
	}
	if running, _ := daemon.IsRunning(sock); running {
		t.Fatal("daemon must no longer hold the flock after stop")
	}
	// A second status says not running.
	st := daemonStatusCmd(&sock)
	var sout bytes.Buffer
	st.SetOut(&sout)
	st.SetArgs(nil)
	if err := st.Execute(); err == nil {
		t.Fatalf("status after stop must exit non-zero, got nil (out=%q)", sout.String())
	}
	if !strings.Contains(sout.String(), "not running") {
		t.Fatalf("status after stop expected 'not running', got %q", sout.String())
	}
}
