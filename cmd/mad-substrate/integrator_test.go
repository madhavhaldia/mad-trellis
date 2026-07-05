package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	for _, want := range []string{"integrator", "run", "--", "claude"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q from integrator-run command: %q", want, got)
		}
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
	for _, want := range []string{"integrator", "run", "--", "codex"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q from integrator-run command: %q", want, got)
		}
	}
}

// TestStartIntegratorPrintOnly: the factored startIntegrator with printOnly=true
// returns opened=false (it prints, opens no terminal) without needing a daemon.
// The printed command must enter the new `integrator run` wrapper; the wrapper
// performs the actual wiring inside the opened terminal.
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
	if _, err := os.Stat(filepath.Join(repo, ".mcp.json")); !os.IsNotExist(err) {
		t.Errorf("integrator start should leave wiring to integrator run; .mcp.json stat err=%v", err)
	}
	for _, want := range []string{shellQuote(repo), "integrator", "run", "--", "claude"} {
		if !strings.Contains(got, want) {
			t.Errorf("printed command missing %q: %q", want, got)
		}
	}
}

func TestBuildIntegratorAgentArgsAppendsRolePromptUnlessUserProvidedPrompt(t *testing.T) {
	got := buildIntegratorAgentArgs("claude", nil, []string{"--model", "opus"})
	if len(got) == 0 || got[len(got)-1] != defaultIntegratorPrompt {
		t.Fatalf("default role prompt must be appended when no positional prompt is present; got %#v", got)
	}

	got = buildIntegratorAgentArgs("claude", []string{"-c", "x=y"}, []string{"review this branch manually"})
	if got[len(got)-1] == defaultIntegratorPrompt {
		t.Fatalf("default role prompt must be skipped when passArgs already contain a positional prompt; got %#v", got)
	}
	if !strings.Contains(strings.Join(got, "\x00"), "review this branch manually") {
		t.Fatalf("user positional prompt was not preserved: %#v", got)
	}
}

func TestIntegratorKeepaliveRestartsAfterCrashAndStopsOnCleanExit(t *testing.T) {
	var calls int
	var sleeps []time.Duration
	opts := integratorRunOptions{
		Keepalive:   true,
		Out:         io.Discard,
		Err:         io.Discard,
		RapidWindow: time.Minute,
		RunOnce: func() (int, error) {
			calls++
			if calls == 1 {
				return 2, nil
			}
			return 0, nil
		},
		Inspect: func(string) (bool, string, bool) { return false, "", true },
		Sleep:   func(d time.Duration) { sleeps = append(sleeps, d) },
		WaitForRetry: func() error {
			t.Fatal("retry prompt should not be reached after one crash")
			return nil
		},
		Now: time.Now,
	}

	code, err := runIntegratorKeepalive(opts)
	if err != nil {
		t.Fatalf("runIntegratorKeepalive: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want clean final exit 0", code)
	}
	if calls != 2 {
		t.Fatalf("RunOnce calls = %d, want 2", calls)
	}
	if len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Fatalf("backoff sleeps = %v, want [1s]", sleeps)
	}
}

func TestIntegratorKeepaliveCleanExitDoesNotRestart(t *testing.T) {
	var calls int
	opts := integratorRunOptions{
		Keepalive: true,
		Out:       io.Discard,
		Err:       io.Discard,
		RunOnce: func() (int, error) {
			calls++
			return 0, nil
		},
		Inspect: func(string) (bool, string, bool) { return false, "", true },
		Sleep: func(time.Duration) {
			t.Fatal("clean exit must not sleep for restart")
		},
		WaitForRetry: func() error {
			t.Fatal("clean exit must not park for retry")
			return nil
		},
		Now: time.Now,
	}

	code, err := runIntegratorKeepalive(opts)
	if err != nil {
		t.Fatalf("runIntegratorKeepalive: %v", err)
	}
	if code != 0 || calls != 1 {
		t.Fatalf("code=%d calls=%d, want code 0 and one call", code, calls)
	}
}

func TestIntegratorKeepaliveParksAfterThreeRapidCrashes(t *testing.T) {
	var calls int
	var retries int
	var out bytes.Buffer
	fixedNow := time.Unix(1000, 0)
	opts := integratorRunOptions{
		Keepalive:   true,
		Out:         &out,
		Err:         io.Discard,
		RapidWindow: time.Minute,
		RunOnce: func() (int, error) {
			calls++
			if calls <= 3 {
				return 9, nil
			}
			return 0, nil
		},
		Inspect: func(string) (bool, string, bool) { return false, "", true },
		Sleep:   func(time.Duration) {},
		WaitForRetry: func() error {
			retries++
			return nil
		},
		Now: func() time.Time { return fixedNow },
	}

	code, err := runIntegratorKeepalive(opts)
	if err != nil {
		t.Fatalf("runIntegratorKeepalive: %v", err)
	}
	if code != 0 || calls != 4 {
		t.Fatalf("code=%d calls=%d, want final clean exit after retry", code, calls)
	}
	if retries != 1 {
		t.Fatalf("retry waits = %d, want 1", retries)
	}
	if !strings.Contains(out.String(), "integrator crashed repeatedly — press enter to retry, ctrl-c to quit") {
		t.Fatalf("missing repeated-crash prompt: %q", out.String())
	}
}

func TestIntegratorKeepaliveStopsWhenAnotherIntegratorAppears(t *testing.T) {
	var calls int
	var out bytes.Buffer
	opts := integratorRunOptions{
		Socket:    "/tmp/mad.sock",
		Keepalive: true,
		Out:       &out,
		Err:       io.Discard,
		RunOnce: func() (int, error) {
			calls++
			return 3, nil
		},
		Inspect: func(socket string) (bool, string, bool) {
			if socket != "/tmp/mad.sock" {
				t.Fatalf("inspect socket = %q", socket)
			}
			return true, "s-other", true
		},
		Sleep: func(time.Duration) {
			t.Fatal("must not sleep/restart when another integrator holds presence")
		},
		WaitForRetry: func() error {
			t.Fatal("must not park when another integrator holds presence")
			return nil
		},
		Now: time.Now,
	}

	code, err := runIntegratorKeepalive(opts)
	if err != nil {
		t.Fatalf("runIntegratorKeepalive: %v", err)
	}
	if code != 0 || calls != 1 {
		t.Fatalf("code=%d calls=%d, want clean wrapper exit without restart", code, calls)
	}
	if !strings.Contains(out.String(), "another integrator is now running (holder s-other); exiting") {
		t.Fatalf("missing zombie-guard message: %q", out.String())
	}
}
