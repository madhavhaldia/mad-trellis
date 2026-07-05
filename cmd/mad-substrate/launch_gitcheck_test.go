package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestCwdInGitRepo is the precondition detection both the launch and integrator
// fail-fast guards share: false in a plain directory, true after `git init`.
func TestCwdInGitRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if cwdInGitRepo() {
		t.Fatalf("a plain temp dir must not be reported as a git repo")
	}

	if out, err := exec.Command("git", "init").CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v (%s)", err, out)
	}
	if !cwdInGitRepo() {
		t.Fatalf("after `git init`, cwd must be reported as a git repo")
	}
}

// TestIntegratorStartFailsFastOutsideRepo proves the integrator guard: in a
// non-repo directory `integrator start` returns the actionable error WITHOUT
// wiring config or opening a terminal (the error is returned before either).
func TestIntegratorStartFailsFastOutsideRepo(t *testing.T) {
	t.Chdir(t.TempDir())

	cmd := integratorStartCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--print"}) // --print would avoid a terminal even if reached

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("integrator start in a non-repo must error, got success: %s", out.String())
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("error must name the not-a-git-repository cause, got: %v", err)
	}
}

func TestIntegratorRunFailsFastOutsideRepo(t *testing.T) {
	t.Chdir(t.TempDir())

	cmd := integratorRunCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--no-keepalive", "--", "sh", "-c", "exit 0"})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("integrator run in a non-repo must error, got success: %s", out.String())
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("error must name the not-a-git-repository cause, got: %v", err)
	}
}
