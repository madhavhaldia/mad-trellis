package runtimecfg

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPerRepoRuntimeDir(t *testing.T) {
	if PerRepoRuntimeDir("") != "" {
		t.Fatal("empty repoRoot must yield empty")
	}
	if PerRepoRuntimeDir("   ") != "" {
		t.Fatal("blank repoRoot must yield empty")
	}

	a := PerRepoRuntimeDir("/Users/x/proj-a")
	a2 := PerRepoRuntimeDir("/Users/x/proj-a")
	b := PerRepoRuntimeDir("/Users/x/proj-b")

	if a == "" {
		t.Fatal("a real repoRoot must resolve to a dir")
	}
	if a != a2 {
		t.Fatalf("must be stable for the same repo: %q vs %q", a, a2)
	}
	if a == b {
		t.Fatal("distinct repos must get distinct runtime dirs")
	}
	if PerRepoRuntimeDir("  /Users/x/proj-a  ") != a {
		t.Fatal("must trim surrounding whitespace so cwd-derived paths stay stable")
	}

	base := filepath.Join(homeBaseDir(), "repos")
	if !strings.HasPrefix(a, base+string(filepath.Separator)) {
		t.Fatalf("per-repo dir %q must live under %q", a, base)
	}
	leaf := filepath.Base(a)
	if len(leaf) != 16 {
		t.Fatalf("hash leaf %q len=%d, want 16 hex", leaf, len(leaf))
	}
	for _, r := range leaf {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("hash leaf %q must be lowercase hex", leaf)
		}
	}
}
