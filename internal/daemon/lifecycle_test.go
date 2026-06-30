package daemon

// Lifecycle hardening tests (chafe C14): the FLOCK is the single-instance truth
// and the PIDFILE is the authoritative pid. Hermetic — scratch sockets in /tmp,
// no real daemon / ~/.mad-substrate touched.

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// A successful Start writes the authoritative pidfile (this process's pid); Close
// removes it.
func TestPidfileWrittenOnStartRemovedOnClose(t *testing.T) {
	path := tmpSock(t)
	d := New(Options{SocketPath: path})
	if err := d.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pf := PidfilePath(path)
	pid, present, err := ReadPidfile(pf)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if !present {
		t.Fatal("pidfile must exist after a successful Start")
	}
	if pid != os.Getpid() {
		t.Fatalf("pidfile must record the daemon's own pid %d, got %d", os.Getpid(), pid)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, present, _ := ReadPidfile(pf); present {
		t.Fatal("pidfile must be removed on Close")
	}
}

// IsRunning reflects the flock: true while a live daemon holds it, false once
// closed.
func TestIsRunningTracksFlock(t *testing.T) {
	path := tmpSock(t)
	if running, err := IsRunning(path); err != nil || running {
		t.Fatalf("no daemon: want running=false err=nil, got running=%v err=%v", running, err)
	}
	d := New(Options{SocketPath: path})
	if err := d.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if running, err := IsRunning(path); err != nil || !running {
		t.Fatalf("live daemon: want running=true err=nil, got running=%v err=%v", running, err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if running, err := IsRunning(path); err != nil || running {
		t.Fatalf("after close: want running=false err=nil, got running=%v err=%v", running, err)
	}
}

// A STALE pidfile (no live daemon holds the flock) is cleaned up by IsRunning,
// which still reports not-running — never an error.
func TestIsRunningCleansStalePidfile(t *testing.T) {
	path := tmpSock(t)
	// Manufacture a stale pidfile with no daemon running (flock is free).
	if err := writePidfile(PidfilePath(path), 999999); err != nil {
		t.Fatalf("seed pidfile: %v", err)
	}
	running, err := IsRunning(path)
	if err != nil {
		t.Fatalf("IsRunning errored on a stale pidfile: %v", err)
	}
	if running {
		t.Fatal("a stale pidfile (flock free) must report not-running")
	}
	if _, present, _ := ReadPidfile(PidfilePath(path)); present {
		t.Fatal("IsRunning must clean up a stale pidfile when no daemon holds the flock")
	}
}

// ReadPidfile distinguishes absent (no error) from garbled (error).
func TestReadPidfileAbsentVsGarbled(t *testing.T) {
	path := tmpSock(t)
	pf := PidfilePath(path)
	if pid, present, err := ReadPidfile(pf); pid != 0 || present || err != nil {
		t.Fatalf("absent pidfile: want (0,false,nil), got (%d,%v,%v)", pid, present, err)
	}
	if err := os.WriteFile(pf, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(pf) })
	if _, present, err := ReadPidfile(pf); !present || err == nil {
		t.Fatalf("garbled pidfile: want present=true and a non-nil error, got present=%v err=%v", present, err)
	}
	// A well-formed pidfile round-trips.
	if err := writePidfile(pf, 4242); err != nil {
		t.Fatal(err)
	}
	if pid, present, err := ReadPidfile(pf); pid != 4242 || !present || err != nil {
		t.Fatalf("valid pidfile: want (4242,true,nil), got (%d,%v,%v)", pid, present, err)
	}
}

// The pidfile path is the documented sibling of the socket.
func TestPidfilePathSibling(t *testing.T) {
	if got := PidfilePath("/tmp/x.sock"); got != "/tmp/x.sock.pid" {
		t.Fatalf("PidfilePath: got %q", got)
	}
	if got := LockPath("/tmp/x.sock"); got != "/tmp/x.sock.lock" {
		t.Fatalf("LockPath: got %q", got)
	}
}

// The pidfile contents are exactly the decimal pid (plus a trailing newline) —
// guards the format the CLI parses.
func TestPidfileFormat(t *testing.T) {
	path := tmpSock(t)
	d := New(Options{SocketPath: path})
	if err := d.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Close()
	b, err := os.ReadFile(PidfilePath(path))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pidfile content %q is not the bare pid", string(b))
	}
}
