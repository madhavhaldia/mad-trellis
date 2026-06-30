package substrate

// Tests for ExecGate — the conductor's GateRunner seam for the CONTAINER grain.
// The arg-injection guards are HERMETIC (no runtime: they reject before invoking
// the CLI); the in-container execution is GATED on the Apple `container` runtime
// and SKIPS-with-a-reason when it is unavailable (CI / a runtime-less host).

import (
	"strings"
	"testing"
)

// TestExecGateRejectsUnsafeID is hermetic (no runtime needed): an empty or
// flag-shaped container id is rejected BEFORE the `container` CLI is ever invoked,
// closing arg-injection via the id (the gate string itself is a single sh -c arg).
func TestExecGateRejectsUnsafeID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"flag-shaped", "--privileged"},
		{"flag-shaped-short", "-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, err := ExecGate(tc.id, "echo should-not-run")
			if err == nil {
				t.Fatalf("ExecGate(%q) must reject the unsafe id, got nil err", tc.id)
			}
			if code != -1 {
				t.Fatalf("rejected id must report exit code -1 (no process ran), got %d", code)
			}
			if out != nil {
				t.Fatalf("rejected id must produce no output, got %q", out)
			}
		})
	}
}

// TestExecGateRunsInsideContainer is the LIVE assertion (runtime-gated): a gate
// runs INSIDE a provisioned boundary container via `container exec ... sh -c`,
// resolving the real exit code from the process state.
//
//   - exit 0  ⇒ code 0, the gate's stdout captured (proof it ran in the container).
//   - exit 3  ⇒ code 3 (non-zero), output captured — the StatusGateFailed path.
//
// The non-vacuous control is structural: the SAME helper returns DIFFERENT codes
// for a passing vs failing gate, so a stub that always returned 0 would fail the
// exit-3 assertion.
func TestExecGateRunsInsideContainer(t *testing.T) {
	requireContainerRuntime(t)
	s := newContainerSub(t)

	spec, err := s.Provision("s-gate-aaaa", Request{Ports: 1})
	if err != nil {
		t.Fatalf("provision (container grain): %v", err)
	}
	cid := spec.containerID
	if cid == "" {
		t.Fatal("container grain must capture a container id")
	}
	t.Cleanup(func() { rmContainer(cid) })

	// Passing gate: exit 0, and its stdout proves it executed inside the container.
	code, out, err := ExecGate(cid, "echo gate-ran-inside && exit 0")
	if err != nil {
		t.Fatalf("ExecGate passing: unexpected err: %v (out=%q)", err, out)
	}
	if code != 0 {
		t.Fatalf("passing gate must report exit code 0, got %d", code)
	}
	if !strings.Contains(string(out), "gate-ran-inside") {
		t.Fatalf("passing gate output must be captured from inside the container, got %q", out)
	}

	// Failing gate: a NON-ZERO exit must surface as a non-zero code (the conductor
	// maps this to StatusGateFailed). Same helper, different outcome ⇒ non-vacuous.
	code, _, _ = ExecGate(cid, "exit 3")
	if code != 3 {
		t.Fatalf("failing gate must report its real exit code 3, got %d", code)
	}
}
