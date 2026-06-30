package launcher

import (
	"errors"
	"testing"
)

// These cover the C34 / shim-loop fix: Run resolves the agent on the AUTHORITATIVE
// grain (spec.Grain), and fails CLOSED on a requested-container/host-daemon mismatch.

func TestRunResolvesHostAgentViaResolver(t *testing.T) {
	f := &fakeConn{whoami: "s-1-abc", provision: okSpec()} // worktree grain
	dial := func(string) (Conn, error) { return f, nil }
	sp := &recordingSpawn{}
	var called string
	cfg := Config{
		Agent: "claude", Dial: dial, Spawn: sp.fn,
		ResolveAgent: func(a string) (string, error) { called = a; return "/real/" + a, nil },
	}
	if _, err := Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called != "claude" {
		t.Fatalf("ResolveAgent called with %q, want claude", called)
	}
	if sp.agent != "/real/claude" {
		t.Fatalf("spawn agent = %q, want the resolved /real/claude", sp.agent)
	}
}

func TestRunHostAgentResolveErrorBlocks(t *testing.T) {
	f := &fakeConn{whoami: "s-1-abc", provision: okSpec()} // worktree grain
	dial := func(string) (Conn, error) { return f, nil }
	sp := &recordingSpawn{}
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn,
		ResolveAgent: func(a string) (string, error) { return "", errors.New("not found") }})
	if code != BlockedExitCode || err == nil {
		t.Fatalf("host resolve failure must BLOCK; got code=%d err=%v", code, err)
	}
	if sp.called {
		t.Fatal("agent must NOT run when host resolution fails (fail-closed)")
	}
}

// TestRunContainerAgentPassesThrough is the C34 fix: at the container grain the
// agent is NOT host-resolved (an in-container-only path passes through untouched).
func TestRunContainerAgentPassesThrough(t *testing.T) {
	f := &fakeConn{whoami: "s-1-abc", provision: containerSpec()} // container grain
	dial := func(string) (Conn, error) { return f, nil }
	sp := &recordingSpawn{}
	resolverCalled := false
	cfg := Config{
		Agent: "/opt/only-in-image", Dial: dial, Spawn: sp.fn,
		RequestedContainerGrain: true,
		ResolveAgent:            func(a string) (string, error) { resolverCalled = true; return "", errors.New("must not be called") },
	}
	if _, err := Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resolverCalled {
		t.Fatal("container grain must NOT host-resolve the agent (C34)")
	}
	if sp.agent != "/opt/only-in-image" {
		t.Fatalf("container agent passed through wrong: %q", sp.agent)
	}
	if sp.target.Grain != "container" {
		t.Fatalf("grain = %q, want container", sp.target.Grain)
	}
}

// TestRunBlocksContainerRequestOnWorktreeDaemon is the shim-loop/mismatch fix: a
// container request the daemon cannot honor fails CLOSED, never a silent host run.
func TestRunBlocksContainerRequestOnWorktreeDaemon(t *testing.T) {
	f := &fakeConn{whoami: "s-1-abc", provision: okSpec()} // worktree grain (the mismatch)
	dial := func(string) (Conn, error) { return f, nil }
	sp := &recordingSpawn{}
	code, err := Run(Config{Agent: "claude", Dial: dial, Spawn: sp.fn,
		RequestedContainerGrain: true,
		ResolveAgent:            func(a string) (string, error) { return "/real/" + a, nil }})
	if code != BlockedExitCode || err == nil {
		t.Fatalf("container-request vs worktree-daemon mismatch must BLOCK; got code=%d err=%v", code, err)
	}
	if sp.called {
		t.Fatal("agent must NOT run on a grain mismatch (fail-closed; no shim-loop)")
	}
}
