package conformance

import (
	"strings"
	"testing"
)

// TestDaemonEnvOptsInToParentDeath proves the harness wires every scratch daemon
// into the parent-death watchdog: the env it builds for the daemon subprocess
// carries MAD_EXIT_WITH_PARENT=1, so an interrupted/killed `conform` run
// never strands its hermetic scratch daemon.
//
// A bare Scratch is enough — daemonEnv()/env() only read the process environment
// and the scratch dirs; no daemon is booted here. The ACTUAL kill-the-parent exit
// is covered by the daemon's watchdog goroutine plus the exitWithParent env-gate
// unit test (cmd/mad-trellis); we deliberately avoid a fork-and-kill test here to
// keep the suite non-flaky.
func TestDaemonEnvOptsInToParentDeath(t *testing.T) {
	s := &Scratch{}
	const want = "MAD_EXIT_WITH_PARENT=1"

	found := false
	for _, kv := range s.daemonEnv() {
		if kv == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("daemon env missing %q (got %v)", want, redact(s.daemonEnv()))
	}

	// The opt-in is daemon-scoped: the shared client env must NOT carry it, so it
	// is unambiguously the daemon that self-terminates with the parent.
	for _, kv := range s.env() {
		if strings.HasPrefix(kv, "MAD_EXIT_WITH_PARENT=") {
			t.Fatalf("client env unexpectedly carries %q", kv)
		}
	}
}

// redact keeps only the mad-trellis-relevant keys so a failure message is readable
// (the inherited os.Environ() is large and noisy).
func redact(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "MAD_") {
			out = append(out, kv)
		}
	}
	return out
}
