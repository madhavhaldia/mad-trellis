package launcher

import (
	"os"
	"testing"
)

// TestMain defaults MAD_CONDUCTOR=off for the WHOLE package. The launcher
// tests run with os.Getwd() inside a real git worktree on a real branch and with
// no mad-substrate.json, so the conductor would otherwise fire by DEFAULT (enabled)
// on every clean-exit case. The pre-conductor tests assert teardown ordering on a
// SINGLE shared fake Conn, and the fake `dial` hands that same Conn back for the
// conductor's FRESH dial too — so leaving auto-converge on would let the
// conductor's `defer conn.Close()` pollute the session conn's recorded call log
// and break TestCleanExitTeardownReleasesOwnLeasesAndBoundary's release<teardown<
// close ordering. The conductor is a feature governed by manifest + env that those
// legacy tests never intended to exercise; defaulting it off here does NOT change
// what they assert. The conductor-trigger tests below re-enable it explicitly.
func TestMain(m *testing.M) {
	os.Setenv("MAD_CONDUCTOR", "off")
	os.Exit(m.Run())
}

// TestConductorShouldRun is the exhaustive truth table for the pure trigger
// predicate: it fires on a clean (code 0, no spawn error) exit with the conductor
// enabled — for BOTH grains (the container branch is fetched from the clone before
// the merge). A non-zero exit, a drop, a spawn error, or a disabled conductor each
// flip it off.
func TestConductorShouldRun(t *testing.T) {
	clean := error(nil)
	cases := []struct {
		name    string
		code    int
		serr    error
		grain   string
		enabled bool
		want    bool
	}{
		{"clean worktree enabled", 0, clean, "worktree", true, true},
		{"clean empty-grain enabled", 0, clean, "", true, true}, // "" is the pre-grain worktree wire
		{"nonzero exit", 3, clean, "worktree", true, false},
		{"drop (128+SIGINT)", 130, clean, "worktree", true, false},
		{"spawn error", 0, errSpawn, "worktree", true, false},
		{"container grain", 0, clean, "container", true, true}, // now fires: branch is fetched from the clone
		{"container grain disabled", 0, clean, "container", false, false},
		{"container grain nonzero", 3, clean, "container", true, false},
		{"conductor disabled", 0, clean, "worktree", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conductorShouldRun(tc.code, tc.serr, tc.grain, tc.enabled); got != tc.want {
				t.Fatalf("conductorShouldRun(%d, %v, %q, %v) = %v, want %v",
					tc.code, tc.serr, tc.grain, tc.enabled, got, tc.want)
			}
		})
	}
}

// errSpawn is a sentinel non-nil spawn error for the predicate table.
var errSpawn = os.ErrClosed

// TestSelectConductorGate is the hermetic (NO runtime) assertion that the launcher
// SELECTS the right gate wiring per grain: the worktree grain leaves GateRunner nil
// (the conductor's host `sh -c` default) and never fetches From; the container grain
// fetches From=clone and selects a NON-NIL GateRunner (substrate.ExecGate inside the
// boundary) when a gate + container id are present. A container grain with no id must
// SKIP the gate — never silently fall back to the host default on the clone.
func TestSelectConductorGate(t *testing.T) {
	const branch = "nm/s-7-abc"
	const clone = "/host/clone"
	const cid = "container-xyz"
	const gate = "make conform"

	t.Run("worktree leaves runner nil and no From", func(t *testing.T) {
		from, eff, runner := selectConductorGate(worktreeGrainName, "", "", branch, gate, nil)
		if runner != nil {
			t.Fatal("worktree grain must leave GateRunner nil (host sh -c default)")
		}
		if from != "" {
			t.Fatalf("worktree grain must not set From, got %q", from)
		}
		if eff != gate {
			t.Fatalf("worktree grain must pass the gate through unchanged, got %q", eff)
		}
	})

	t.Run("empty grain (pre-grain wire) leaves runner nil", func(t *testing.T) {
		_, _, runner := selectConductorGate(hostGrainName, "", "", branch, gate, nil)
		if runner != nil {
			t.Fatal("pre-grain wire must leave GateRunner nil (host default)")
		}
	})

	t.Run("container with gate+id selects a non-nil runner and From=clone", func(t *testing.T) {
		from, eff, runner := selectConductorGate(containerGrainName, cid, clone, branch, gate, nil)
		if runner == nil {
			t.Fatal("container grain with a gate + container id must select a non-nil GateRunner")
		}
		if from != clone {
			t.Fatalf("container grain must fetch From the clone %q, got %q", clone, from)
		}
		if eff != gate {
			t.Fatalf("container grain must keep the gate %q, got %q", gate, eff)
		}
	})

	t.Run("container with no gate is merge-only (runner nil)", func(t *testing.T) {
		from, eff, runner := selectConductorGate(containerGrainName, cid, clone, branch, "", nil)
		if runner != nil {
			t.Fatal("container grain with no gate must leave GateRunner nil (merge-only)")
		}
		if eff != "" {
			t.Fatalf("merge-only must keep the gate empty, got %q", eff)
		}
		if from != clone {
			t.Fatalf("container grain must still fetch From the clone, got %q", from)
		}
	})

	// NON-VACUOUS CONTROL: a container grain with a gate but NO container id must NOT
	// fall back to the host `sh -c` default (which would validate the host clone, not
	// the container). It skips the gate (effGate="" , runner nil) — merge-only.
	t.Run("container with gate but no id skips the gate (no host fallback)", func(t *testing.T) {
		_, eff, runner := selectConductorGate(containerGrainName, "", clone, branch, gate, nil)
		if runner != nil {
			t.Fatal("container grain with no id must not select a runner")
		}
		if eff != "" {
			t.Fatalf("container grain with no id must SKIP the gate (effGate=\"\"), got %q — host fallback would run on the clone", eff)
		}
	})
}

// TestConductorSafeBranch is the defensive arg-injection guard at the launcher
// boundary: reject empty, a flag-looking ref, and any ref with disallowed bytes;
// accept ordinary branch names.
func TestConductorSafeBranch(t *testing.T) {
	bad := []string{"", "-x", "--force", "a b", "feat/x;rm -rf", "tab\tname"}
	for _, s := range bad {
		if conductorSafeBranch(s) {
			t.Errorf("conductorSafeBranch(%q) = true, want false (unsafe)", s)
		}
	}
	good := []string{"feat/desktop-setup", "nm/s-1", "main", "release/v1.2.3", "a_b.c"}
	for _, s := range good {
		if !conductorSafeBranch(s) {
			t.Errorf("conductorSafeBranch(%q) = false, want true (safe)", s)
		}
	}
}

// TestConductorFiresOnCleanWorktreeExit is the best-effort integration test. It
// drives a full Run with a fake Dial/Conn and a fake Spawn returning (0, nil) on
// the worktree grain, and asserts the conductor fired by observing its FRESH DIAL
// — on a clean exit Run dials TWICE (once for the session, once more for the
// conductor's own connection); on a drop only once. We assert on the dial count
// rather than a specific RPC because the conductor now runs read-only git guards
// (drift / nothing-to-converge) BEFORE any daemon call, so in this hermetic env
// (no real convergeable branch) it short-circuits before classify.route — the
// fresh dial is the trigger-fired signal that survives that reordering. The drop
// case (next test) is the non-vacuous control.
func TestConductorFiresOnCleanWorktreeExit(t *testing.T) {
	t.Setenv("MAD_CONDUCTOR", "on") // re-enable (TestMain defaults it off); "on" is not a disable value
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec()}
	dials := 0
	dial := func(string) (Conn, error) { dials++; return conn, nil }
	sp := &recordingSpawn{code: 0}
	if _, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sp.called {
		t.Fatal("vacuous: spawn was never reached under full governance")
	}
	if dials != 2 {
		t.Fatalf("conductor did not dial a fresh connection on a clean worktree exit; want 2 dials (session+conductor), got %d", dials)
	}
}

// TestConductorDoesNotFireOnDrop is the non-vacuous control for the test above: a
// terminal drop returns 128+signum (130 = SIGINT), which conductorShouldRun must
// reject — so the conductor's fresh dial must NOT happen (only the session dial).
// Without the code==0 guard this flips red, proving the clean-exit gate is real.
func TestConductorDoesNotFireOnDrop(t *testing.T) {
	t.Setenv("MAD_CONDUCTOR", "on")
	conn := &fakeConn{whoami: "s-1-abc", provision: okSpec()}
	dials := 0
	dial := func(string) (Conn, error) { dials++; return conn, nil }
	sp := &recordingSpawn{code: 130} // 128 + SIGINT: a terminal drop, NOT a clean exit
	if _, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dials != 1 {
		t.Fatalf("conductor fired on a DROP (code 130): want only the session dial (1), got %d (an extra dial means the conductor ran)", dials)
	}
}
