package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeExec creates an executable file at dir/name and returns its path.
func writeExec(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAgentFromArgv0(t *testing.T) {
	cases := map[string]string{
		"claude":                 "claude",
		"/usr/local/bin/codex":   "codex",
		"./claude":               "claude",
		"mad-substrate":          "", // normal invocation
		"/opt/bin/mad-substrate": "", // normal invocation by path
		"git":                    "", // unsupported tool
		"":                       "", // degenerate
	}
	for argv0, want := range cases {
		if got := AgentFromArgv0(argv0); got != want {
			t.Errorf("AgentFromArgv0(%q) = %q, want %q", argv0, got, want)
		}
	}
}

// ResolveReal must find the REAL agent and never resolve back into the shim dir
// (which would re-exec mad-substrate forever). The "self" binary stands in for the
// running mad-substrate; the shim is a symlink to it.
func TestResolveRealExcludesShimDir(t *testing.T) {
	root := t.TempDir()
	self := writeExec(t, root, "mad-substrate") // the running binary
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "realbin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// shim: claude → mad-substrate; real: a genuine claude.
	if err := os.Symlink(self, filepath.Join(shimDir, "claude")); err != nil {
		t.Fatal(err)
	}
	realClaude := writeExec(t, realDir, "claude")

	// PATH puts the shim FIRST (as a real interposed PATH would) then the real dir.
	path := shimDir + string(os.PathListSeparator) + realDir
	got, err := ResolveReal("claude", shimDir, self, path)
	if err != nil {
		t.Fatalf("ResolveReal: %v", err)
	}
	if got != realClaude {
		t.Fatalf("resolved %q, want the REAL claude %q (must skip the shim)", got, realClaude)
	}
}

// FAIL-CLOSED: if the only reachable claude resolves back to the mad-substrate shim,
// ResolveReal must refuse rather than loop or fall through to an ungoverned exec.
func TestResolveRealTamperedOnlyShimFailsClosed(t *testing.T) {
	root := t.TempDir()
	self := writeExec(t, root, "mad-substrate")
	shimDir := filepath.Join(root, "shims")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(shimDir, "claude")); err != nil {
		t.Fatal(err)
	}
	// A second PATH entry that ALSO only contains a symlink-to-self (a stray shim
	// not equal to shimDir): the resolver must detect it points at mad-substrate and
	// refuse, never exec it.
	strayDir := filepath.Join(root, "stray")
	if err := os.MkdirAll(strayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(strayDir, "claude")); err != nil {
		t.Fatal(err)
	}

	path := shimDir + string(os.PathListSeparator) + strayDir
	_, err := ResolveReal("claude", shimDir, self, path)
	if !errors.Is(err, ErrShimTampered) {
		t.Fatalf("an all-shim PATH must fail closed with ErrShimTampered; got %v", err)
	}
}

// FAIL-CLOSED on an unknown self (the cardinal rule): if the running mad-substrate
// binary cannot be identified (os.Executable() failed → selfBin==""), ResolveReal
// must NOT silently disable its shim-loop detector and return a shim — that would
// re-exec mad-substrate forever. It must refuse. This pins the os.Executable()-error
// branch that resolveAgentBinary now turns into a BLOCK.
func TestResolveRealEmptySelfFailsClosed(t *testing.T) {
	root := t.TempDir()
	self := writeExec(t, root, "mad-substrate")
	// A PATH whose only `claude` is a symlink → mad-substrate (a shim). With a known
	// self this fails closed (ErrShimTampered); with an EMPTY self the loop guard
	// must STILL refuse rather than return the shim.
	strayDir := filepath.Join(root, "stray")
	if err := os.MkdirAll(strayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(strayDir, "claude")); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveReal("claude", filepath.Join(root, "shims"), "", strayDir)
	if err == nil {
		t.Fatalf("empty self must fail closed, but ResolveReal returned %q (would re-exec the shim forever)", got)
	}
	if !errors.Is(err, ErrShimTampered) {
		t.Fatalf("empty self should yield ErrShimTampered; got %v", err)
	}
}

func TestResolveRealNotFoundFailsClosed(t *testing.T) {
	root := t.TempDir()
	self := writeExec(t, root, "mad-substrate")
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveReal("claude", filepath.Join(root, "shims"), self, empty)
	if !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("no agent on PATH must fail closed with ErrAgentNotFound; got %v", err)
	}
}

func TestResolveRealSkipsNonExecutable(t *testing.T) {
	root := t.TempDir()
	self := writeExec(t, root, "mad-substrate")
	d1 := filepath.Join(root, "d1")
	d2 := filepath.Join(root, "d2")
	for _, d := range []string{d1, d2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// d1 has a NON-executable claude (must be skipped); d2 has the real one.
	if err := os.WriteFile(filepath.Join(d1, "claude"), []byte("not exec"), 0o644); err != nil {
		t.Fatal(err)
	}
	realClaude := writeExec(t, d2, "claude")
	path := d1 + string(os.PathListSeparator) + d2
	got, err := ResolveReal("claude", filepath.Join(root, "shims"), self, path)
	if err != nil || got != realClaude {
		t.Fatalf("must skip the non-executable and find the real claude; got %q err=%v", got, err)
	}
}

func TestInstallShimsWritesOnlyShims(t *testing.T) {
	root := t.TempDir()
	self := writeExec(t, root, "mad-substrate")
	shimDir := filepath.Join(root, "shims")

	installed, err := InstallShims(self, shimDir, []string{"claude", "codex"})
	if err != nil {
		t.Fatalf("InstallShims: %v", err)
	}
	if len(installed) != 2 {
		t.Fatalf("installed %d shims, want 2", len(installed))
	}
	entries, _ := os.ReadDir(shimDir)
	if len(entries) != 2 {
		t.Fatalf("shim dir has %d entries, want exactly the 2 shims", len(entries))
	}
	for _, a := range []string{"claude", "codex"} {
		resolved, err := filepath.EvalSymlinks(filepath.Join(shimDir, a))
		if err != nil {
			t.Fatalf("shim %s: %v", a, err)
		}
		selfReal, _ := filepath.EvalSymlinks(self)
		if resolved != selfReal {
			t.Errorf("shim %s resolves to %q, want the mad-substrate binary %q", a, resolved, selfReal)
		}
	}

	// Idempotent: installing again replaces, never errors or duplicates.
	if _, err := InstallShims(self, shimDir, []string{"claude"}); err != nil {
		t.Fatalf("re-install should be idempotent: %v", err)
	}
}

// POSITIVE CONTROL: a NAIVE resolver (first PATH match, no shim exclusion) would
// return the shim — exactly the infinite-loop bug. Asserting that the naive pick
// differs from ResolveReal's pick proves the exclusion guard is load-bearing.
func TestResolveRealControlNaiveWouldPickTheShim(t *testing.T) {
	root := t.TempDir()
	self := writeExec(t, root, "mad-substrate")
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "realbin")
	for _, d := range []string{shimDir, realDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(self, filepath.Join(shimDir, "claude")); err != nil {
		t.Fatal(err)
	}
	realClaude := writeExec(t, realDir, "claude")
	path := shimDir + string(os.PathListSeparator) + realDir

	// Naive: first executable match on PATH = the shim.
	naive := naiveFirstMatch("claude", path)
	if filepath.Dir(naive) != shimDir {
		t.Fatalf("control malformed: naive pick %q should be the shim", naive)
	}
	got, err := ResolveReal("claude", shimDir, self, path)
	if err != nil {
		t.Fatalf("ResolveReal: %v", err)
	}
	if got == naive {
		t.Fatal("REGRESSION: ResolveReal returned the shim (would loop) — the exclusion guard is gone")
	}
	if got != realClaude {
		t.Fatalf("ResolveReal = %q, want the real claude", got)
	}
}

func naiveFirstMatch(name, pathEnv string) string {
	for _, entry := range strings.Split(pathEnv, string(os.PathListSeparator)) {
		cand := filepath.Join(entry, name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand
		}
	}
	return ""
}
