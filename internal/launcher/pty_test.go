package launcher

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Ambient-interaction parity self-cert (Inv 13, card-5 [13-interaction]): run the
// REAL PTY plumbing against a fixture agent and assert the contract that makes a
// governed session indistinguishable from a bare one — the env-spec is applied,
// the agent's args are forwarded VERBATIM (no injected prompt/goal step), and the
// exit code propagates exactly. (Full transcript/resize parity is live-validated
// by dogfooding; this removes the "RunPTY never exercised" vacuum.)
func TestRunPTYAppliesEnvAndForwardsArgsVerbatim(t *testing.T) {
	var out bytes.Buffer
	env := map[string]string{"MAD_SESSION": "s-test", "PORT": "5000"}
	// sh -c <script> <name> <arg1>: $0=name, $1=arg1. The child echoes its
	// forwarded arg and two env-spec values; printf (no newline) avoids PTY CRLF.
	code, err := runPTYIO(
		strings.NewReader(""), &out,
		ExecTarget{Cwd: t.TempDir()}, env,
		"sh", []string{"-c", `printf 'ARG=%s SESS=%s PORT=%s' "$1" "$MAD_SESSION" "$PORT"`, "sh", "hello"},
	)
	if err != nil {
		t.Fatalf("runPTYIO: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (clean child exit propagated)", code)
	}
	got := out.String()
	for _, want := range []string{"ARG=hello", "SESS=s-test", "PORT=5000"} {
		if !strings.Contains(got, want) {
			t.Errorf("PTY output %q missing %q (env-spec applied + args verbatim)", got, want)
		}
	}
}

// Exit-code propagation through the real PTY path (not just waitCode in isolation).
func TestRunPTYPropagatesNonZeroExit(t *testing.T) {
	var out bytes.Buffer
	code, err := runPTYIO(strings.NewReader(""), &out, ExecTarget{Cwd: t.TempDir()}, nil, "sh", []string{"-c", "exit 5"})
	if err != nil {
		t.Fatalf("runPTYIO: %v", err)
	}
	if code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
}

// isTerminalDrop classifies the launcher-go-away signals that must drive the
// bounded teardown-on-drop path, and excludes job-control signals (merely relayed).
func TestIsTerminalDropClassification(t *testing.T) {
	for _, s := range []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM} {
		if !isTerminalDrop(s) {
			t.Errorf("%v must be classified as a terminal-drop signal", s)
		}
	}
	for _, s := range []os.Signal{syscall.SIGTSTP, syscall.SIGCONT, syscall.SIGQUIT, syscall.SIGWINCH} {
		if isTerminalDrop(s) {
			t.Errorf("%v must NOT be a terminal-drop signal (relay-only)", s)
		}
	}
}

// THE DROP-PATH FIX (orphan reap, part 1): a SIGHUP delivered to the LAUNCHER — the
// controlling terminal closing or the controlling process dying — must make
// runPTYIO RETURN promptly so Run's clean-exit teardown can fire, EVEN IF the child
// traps/ignores the signal and keeps running. Without the bounded drop grace,
// runPTYIO would block in Wait() forever and the boundary/lease would be orphaned.
//
// The child here TRAPS SIGHUP (and SIGTERM) and sleeps, simulating an agent that
// does not die on the relayed signal. We shrink dropGrace so the test is fast, send
// SIGHUP to our OWN process (which signal.Notify forwards), and assert runPTYIO
// returns within the grace with the conventional 128+SIGHUP code — i.e. teardown is
// now reachable on the drop path.
func TestSIGHUPReturnsWithinGraceWhenChildIgnores(t *testing.T) {
	orig := dropGrace
	dropGrace = 150 * time.Millisecond
	defer func() { dropGrace = orig }()

	var out bytes.Buffer
	done := make(chan struct{})
	var code int
	var rerr error
	go func() {
		// trap HUP/TERM and sleep — a child that does NOT die on the relayed signal.
		code, rerr = runPTYIO(strings.NewReader(""), &out, ExecTarget{Cwd: t.TempDir()}, nil,
			"sh", []string{"-c", "trap '' HUP TERM; sleep 30"})
		close(done)
	}()

	// Give the child a moment to install its trap and start sleeping (and runPTYIO's
	// signal.Notify a moment to arm), then deliver SIGHUP to ourselves; runPTYIO
	// forwards it to the child.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("runPTYIO returned before SIGHUP was even sent")
	default:
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP to self: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runPTYIO did NOT return after SIGHUP despite the child ignoring it — teardown would be orphaned")
	}
	if rerr != nil {
		t.Fatalf("runPTYIO returned an error: %v", rerr)
	}
	if code != 128+int(syscall.SIGHUP) {
		t.Errorf("drop-path exit code = %d, want %d (128+SIGHUP)", code, 128+int(syscall.SIGHUP))
	}
}

// Control (non-vacuity): a child that EXITS on the relayed terminal-drop signal
// still yields its EXACT exit code via the normal Wait path — the drop grace does
// not clobber a clean death, it only backstops a wedged one.
func TestTerminalDropPreservesExactExitWhenChildDies(t *testing.T) {
	orig := dropGrace
	dropGrace = 5 * time.Second // long, so the grace timer never wins this race
	defer func() { dropGrace = orig }()

	var out bytes.Buffer
	done := make(chan struct{})
	var code int
	go func() {
		// Default SIGHUP disposition (no trap): the child dies on the relayed signal,
		// so Wait() returns its real signal-death status before the grace elapses.
		code, _ = runPTYIO(strings.NewReader(""), &out, ExecTarget{Cwd: t.TempDir()}, nil,
			"sh", []string{"-c", "sleep 30"})
		close(done)
	}()
	time.Sleep(150 * time.Millisecond) // let the child start sleeping
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP to self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runPTYIO did not return after the child died on SIGHUP")
	}
	if code != 128+int(syscall.SIGHUP) {
		t.Errorf("exact child signal-death code = %d, want %d (128+SIGHUP)", code, 128+int(syscall.SIGHUP))
	}
}

// MergeEnv: per-agent env-spec values win; the rest of the base inherits; the
// substrate never names a launch-critical var, but if `over` does, it wins (the
// substrate's own validation, not MergeEnv, is what keeps PATH out of `over`).
func TestMergeEnvOverlaysAndInherits(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/x", "PORT=80"}
	over := map[string]string{"PORT": "5000", "MAD_SESSION": "s-1"}
	got := MergeEnv(base, over)

	m := map[string]string{}
	for _, kv := range got {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	if m["PORT"] != "5000" {
		t.Errorf("per-agent PORT must override base; got %q", m["PORT"])
	}
	if m["PATH"] != "/usr/bin" || m["HOME"] != "/home/x" {
		t.Errorf("inherited base vars must pass through; PATH=%q HOME=%q", m["PATH"], m["HOME"])
	}
	if m["MAD_SESSION"] != "s-1" {
		t.Errorf("new per-agent var must be present; got %q", m["MAD_SESSION"])
	}
	// No duplicate PORT entries (the base PORT must be dropped, not appended).
	var portCount int
	for _, kv := range got {
		if strings.HasPrefix(kv, "PORT=") {
			portCount++
		}
	}
	if portCount != 1 {
		t.Errorf("PORT appears %d times, want exactly 1 (base dropped)", portCount)
	}
}

// waitCode propagates a child's EXACT exit status (Inv 13 transparency): a clean
// non-zero exit and signal death both map to the conventional code.
func TestWaitCodePropagatesExactExit(t *testing.T) {
	// Clean non-zero exit.
	err := exec.Command("sh", "-c", "exit 7").Run()
	if code, _ := waitCode(err); code != 7 {
		t.Errorf("exit 7 → code %d, want 7", code)
	}
	// Zero exit.
	if code, werr := waitCode(exec.Command("sh", "-c", "exit 0").Run()); code != 0 || werr != nil {
		t.Errorf("exit 0 → code=%d err=%v, want 0/nil", code, werr)
	}
	// Signal death → 128+signum (SIGTERM=15 → 143).
	if code, _ := waitCode(exec.Command("sh", "-c", "kill -TERM $$").Run()); code != 143 {
		t.Errorf("SIGTERM death → code %d, want 143 (128+15)", code)
	}
}
