package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestReadPidfile(t *testing.T) {
	dir := t.TempDir()
	// Missing file -> not ok.
	if _, ok := readPidfile(filepath.Join(dir, "nope.pid")); ok {
		t.Fatal("missing pidfile must report ok=false")
	}
	// Valid pid.
	p := filepath.Join(dir, "ok.pid")
	if err := os.WriteFile(p, []byte("4321\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, ok := readPidfile(p); !ok || pid != 4321 {
		t.Fatalf("got (%d,%v), want (4321,true)", pid, ok)
	}
	// Garbage / non-positive -> not ok.
	for _, bad := range []string{"not-a-pid", "0", "-5", ""} {
		bp := filepath.Join(dir, "bad.pid")
		if err := os.WriteFile(bp, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readPidfile(bp); ok {
			t.Fatalf("pidfile %q must report ok=false", bad)
		}
	}
}

func TestPidAlive(t *testing.T) {
	// This test process is certainly alive.
	if !pidAlive(os.Getpid()) {
		t.Fatal("current process must be reported alive")
	}
	// Non-positive pids are never alive.
	if pidAlive(0) || pidAlive(-1) {
		t.Fatal("non-positive pids must not be alive")
	}
	// A very high, almost-certainly-unused pid is not alive (ESRCH).
	if pidAlive(2000000000) {
		t.Skip("pid 2000000000 unexpectedly exists on this host; skipping the dead-pid arm")
	}
}

// TestIntegratorStopSlotKeys covers both the default singleton and an opt-in pool,
// and must stay in lockstep with the MCP server's slot keys so stop inspects the
// SAME leases the server acquires.
func TestIntegratorStopSlotKeys(t *testing.T) {
	single := integratorStopSlotKeys(1)
	if len(single) != 1 || string(single[0]) != integratorLeaseKey {
		t.Fatalf("singleton must be exactly the well-known key, got %q", single)
	}
	// N<=1 (including 0 / negatives) collapses to the singleton.
	if got := integratorStopSlotKeys(0); len(got) != 1 || string(got[0]) != integratorLeaseKey {
		t.Fatalf("N=0 must collapse to the singleton, got %q", got)
	}
	pool := integratorStopSlotKeys(3)
	if len(pool) != 3 {
		t.Fatalf("pool of 3 must yield 3 keys, got %d", len(pool))
	}
	for i, k := range pool {
		want := integratorLeaseKey + ":slot-" + strconv.Itoa(i)
		if string(k) != want {
			t.Fatalf("slot %d key = %q, want %q", i, k, want)
		}
	}
}

// TestPidLooksLikeIntegrator is the pid-reuse guard's non-vacuous negative: a live
// process whose command line is plainly NOT an integrator (a spawned `sleep`) must
// be REJECTED, so stop never signals a recycled pid belonging to something else.
// We spawn a child rather than probe os.Getpid() because the go-test process's own
// argv can incidentally contain "integrator" (e.g. a -run filter naming a test).
func TestPidLooksLikeIntegrator(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a child process to probe: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	pid := cmd.Process.Pid
	if !pidAlive(pid) {
		t.Fatalf("spawned child (pid %d) should be alive", pid)
	}
	if pidLooksLikeIntegrator(pid) {
		t.Fatalf("`sleep 60` (pid %d) is not an integrator; guard must reject it", pid)
	}
}
