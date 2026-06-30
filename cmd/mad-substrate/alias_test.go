package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runAlias executes the alias command with args and returns stdout + error.
func runAlias(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := aliasCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestAliasCreatesSymlinkAlongsideBinary(t *testing.T) {
	dir := t.TempDir()
	out, err := runAlias(t, "ms", "--dir", dir)
	if err != nil {
		t.Fatalf("alias: %v\n%s", err, out)
	}
	link := filepath.Join(dir, "ms")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", link, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	// It must point at THIS test binary (os.Executable), so invoking the alias
	// runs the same code — that is the whole point of the convenience link.
	dst, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	self, _ := os.Executable()
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	if dst != self {
		t.Fatalf("alias points at %q, want %q", dst, self)
	}
}

func TestAliasDefaultsToMS(t *testing.T) {
	dir := t.TempDir()
	if _, err := runAlias(t, "--dir", dir); err != nil {
		t.Fatalf("alias (default name): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "ms")); err != nil {
		t.Fatalf("default alias name should be ms: %v", err)
	}
}

func TestAliasIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := runAlias(t, "ms", "--dir", dir); err != nil {
		t.Fatalf("first install: %v", err)
	}
	// A second run must replace the stale symlink, not error out.
	if _, err := runAlias(t, "ms", "--dir", dir); err != nil {
		t.Fatalf("second install (idempotency): %v", err)
	}
}

// TestAliasRejectsShimNames is the non-vacuous guard: an agent-shim argv[0]
// (claude/codex) is intercepted by the launcher and would NOT run the CLI, so it
// must be refused as an alias name; likewise the binary's own name.
func TestAliasRejectsShimNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"claude", "codex", "mad-substrate"} {
		out, err := runAlias(t, name, "--dir", dir)
		if err == nil {
			t.Fatalf("alias %q should be rejected, got success: %s", name, out)
		}
		if _, lerr := os.Lstat(filepath.Join(dir, name)); lerr == nil {
			t.Fatalf("alias %q must not be created", name)
		}
	}
}

func TestAliasRefusesToClobberRealFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "ms")
	if err := os.WriteFile(real, []byte("not a symlink"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runAlias(t, "ms", "--dir", dir)
	if err == nil {
		t.Fatalf("should refuse to clobber a real file without --force: %s", out)
	}
	// --force overrides.
	if _, ferr := runAlias(t, "ms", "--dir", dir, "--force"); ferr != nil {
		t.Fatalf("--force should replace the file: %v", ferr)
	}
	fi, _ := os.Lstat(real)
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("--force should have replaced the file with a symlink")
	}
}

func TestAliasPrintEmitsShellLine(t *testing.T) {
	out, err := runAlias(t, "ms", "--print")
	if err != nil {
		t.Fatalf("alias --print: %v", err)
	}
	if !strings.HasPrefix(out, "alias ms=") {
		t.Fatalf("--print should emit a shell alias line, got: %q", out)
	}
	// --print must not write any file.
	if strings.Contains(out, "installed:") {
		t.Fatalf("--print must not install a symlink: %q", out)
	}
}
