package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what was
// written. startIntegrator (and thus `integrator start`) prints the integrator
// command to os.Stdout, so the print-mode tests capture it here.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func TestRenderIntegratorStatus(t *testing.T) {
	cases := []struct {
		held      bool
		holder    string
		reachable bool
		want      string
	}{
		{false, "", false, "no integrator running (daemon not reachable)"},
		{false, "", true, "no integrator running"},
		{true, "sess-7", true, "integrator running (holder sess-7)"},
		// reachable=false dominates even if held were somehow set.
		{true, "sess-7", false, "no integrator running (daemon not reachable)"},
	}
	for _, c := range cases {
		if got := renderIntegratorStatus(c.held, c.holder, c.reachable); got != c.want {
			t.Errorf("renderIntegratorStatus(%v,%q,%v) = %q, want %q", c.held, c.holder, c.reachable, got, c.want)
		}
	}
}

func TestHostForAgent(t *testing.T) {
	cases := map[string]string{
		"claude":          "claude",
		"codex":           "codex",
		"/usr/bin/codex":  "codex",
		"/opt/bin/claude": "claude",
		"CODEX":           "codex",
		"someother-agent": "claude", // default
		"":                "claude",
	}
	for agent, want := range cases {
		if got := hostForAgent(agent); got != want {
			t.Errorf("hostForAgent(%q) = %q, want %q", agent, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     `'plain'`,
		"has space": `'has space'`,
		"a'b":       `'a'\''b'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOsaQuote(t *testing.T) {
	if got, want := osaQuote(`cd '/a b' && claude`), `"cd '/a b' && claude"`; got != want {
		t.Errorf("osaQuote = %q, want %q", got, want)
	}
	// Backslashes and double quotes are escaped.
	if got, want := osaQuote(`a"b\c`), `"a\"b\\c"`; got != want {
		t.Errorf("osaQuote escaping = %q, want %q", got, want)
	}
}

func TestTerminalCommandString(t *testing.T) {
	dir := "/work/my repo"
	got := terminalCommandString(dir, []string{"claude", "-c", `args=["mcp","--role","integrator"]`})
	// cd into the (quoted) dir, then the (quoted) agent + args.
	if !strings.HasPrefix(got, "cd '/work/my repo' && ") {
		t.Errorf("missing quoted cd prefix: %q", got)
	}
	if !strings.Contains(got, "'claude'") {
		t.Errorf("agent not quoted: %q", got)
	}
	// The arg with double-quotes survives inside POSIX single quotes verbatim.
	if !strings.Contains(got, `'args=["mcp","--role","integrator"]'`) {
		t.Errorf("arg with quotes not preserved: %q", got)
	}
}

func TestOpenIntegratorTerminalPrintOnly(t *testing.T) {
	opened, mech := openIntegratorTerminal("/work", []string{"claude"}, true)
	if opened {
		t.Errorf("printOnly must not open a terminal")
	}
	if mech != "" {
		t.Errorf("printOnly mechanism should be empty, got %q", mech)
	}
}

// TestIntegratorStartPrintMode: `integrator start --print` (no daemon) prints the
// exact command to run — containing `cd <dir>` and the agent — and does NOT error.
func TestIntegratorStartPrintMode(t *testing.T) {
	repo := seedRepo(t)
	t.Chdir(repo)
	// Point at a bogus socket so the presence-check daemon dial fails fast
	// (fail-soft: a warning to stderr, the command still proceeds).
	t.Setenv("MAD_SOCKET", filepath.Join(t.TempDir(), "nope.sock"))

	cmd := integratorStartCmd()
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"--print"})
	var execErr error
	got := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("integrator start --print: %v\nstderr: %s", execErr, errBuf.String())
	}

	if !strings.Contains(got, "cd ") {
		t.Errorf("output missing `cd <dir>`: %q", got)
	}
	if !strings.Contains(got, shellQuote(repo)) {
		t.Errorf("output missing the quoted worktree dir %q: %q", repo, got)
	}
	if !strings.Contains(got, "claude") {
		t.Errorf("output missing default agent `claude`: %q", got)
	}
}

// TestIntegratorStartPrintModeCodex: passthrough selects the agent and the codex
// MCP `-c` wiring is injected into the printed command.
func TestIntegratorStartPrintModeCodex(t *testing.T) {
	repo := seedRepo(t)
	t.Chdir(repo)
	t.Setenv("MAD_SOCKET", filepath.Join(t.TempDir(), "nope.sock"))

	cmd := integratorStartCmd()
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"--print", "--", "codex"})
	var execErr error
	got := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("integrator start --print -- codex: %v\nstderr: %s", execErr, errBuf.String())
	}
	if !strings.Contains(got, "codex") {
		t.Errorf("output missing agent `codex`: %q", got)
	}
	// The role'd MCP args must be wired into the codex command.
	if !strings.Contains(got, `--role`) || !strings.Contains(got, "integrator") {
		t.Errorf("codex command missing integrator role wiring: %q", got)
	}
}

// TestStartIntegratorPrintOnly: the factored startIntegrator with printOnly=true
// wires the integrator config into dir (claude writes .mcp.json) and returns
// opened=false (it prints, opens no terminal) without needing a daemon.
func TestStartIntegratorPrintOnly(t *testing.T) {
	repo := seedRepo(t)
	t.Chdir(repo)

	var opened bool
	var serr error
	got := captureStdout(t, func() {
		opened, serr = startIntegrator("/nonexistent.sock", repo, "claude", nil, true)
	})
	if serr != nil {
		t.Fatalf("startIntegrator: %v", serr)
	}
	if opened {
		t.Errorf("printOnly must not open a terminal (opened=true)")
	}
	// Wiring actually happened: claude's integrator wiring writes .mcp.json.
	if _, err := os.Stat(filepath.Join(repo, ".mcp.json")); err != nil {
		t.Errorf("expected integrator wiring to write .mcp.json in %s: %v", repo, err)
	}
	// The printed command targets the worktree and the agent.
	if !strings.Contains(got, shellQuote(repo)) || !strings.Contains(got, "claude") {
		t.Errorf("printed command missing dir/agent: %q", got)
	}
}
