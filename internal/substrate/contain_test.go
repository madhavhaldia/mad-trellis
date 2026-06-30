package substrate

// Hand-authored GATED invariant tests for the escape-resistance primitive
// (Inv 1 + the Inv-4 "sandbox-harder" tail at the worktree grain). Every
// rejection carries a POSITIVE CONTROL (docs/0004 review protocol: each
// absence-assertion injects the forbidden artifact and proves the check is
// non-vacuous) — here, a naive join that lets the same input escape.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainRejectsTraversalVectors(t *testing.T) {
	base := t.TempDir()
	baseAbs := mustAbs(t, base)

	vectors := []struct {
		name      string
		untrusted string
	}{
		{"absolute", "/etc/passwd"},
		{"parent-traversal", "../sibling/x"},
		{"deep-parent-traversal", "a/b/../../../escape"},
		{"absolute-elsewhere", filepath.Join(os.TempDir(), "elsewhere")},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			if _, err := Contain(base, v.untrusted); err == nil {
				t.Fatalf("Contain must REJECT %q (escape vector)", v.untrusted)
			}
			// Positive control: a naive join of the SAME input escapes base, so the
			// rejection is guarding a real hole — not a vacuous always-reject.
			naive := filepath.Join(baseAbs, v.untrusted)
			if filepath.IsAbs(v.untrusted) {
				naive = v.untrusted // a naive impl that doesn't reject absolutes uses them verbatim
			}
			if withinBase(baseAbs, naive) {
				t.Fatalf("positive control vacuous: naive handling of %q did NOT escape base (%s)", v.untrusted, naive)
			}
		})
	}
}

func TestContainRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	// A path THROUGH the in-base symlink resolves outside base → rejected.
	if _, err := Contain(base, filepath.Join("link", "secret")); err == nil {
		t.Fatal("Contain must REJECT a path that escapes via an in-base symlink")
	}
	// Positive control: the LEXICAL join stays within base, so only symlink
	// RESOLUTION catches this — proving the symlink check (not a lexical accident)
	// is what rejects it.
	naive := filepath.Join(mustAbs(t, base), "link", "secret")
	if !withinBase(mustAbs(t, base), naive) {
		t.Fatal("positive control vacuous: lexical join already escaped; symlink resolution unexercised")
	}
}

func TestContainAcceptsContainedPath(t *testing.T) {
	base := t.TempDir()
	got, err := Contain(base, filepath.Join("res", "db"))
	if err != nil {
		t.Fatalf("Contain must ACCEPT a contained relative path: %v", err)
	}
	if !withinBase(mustAbs(t, base), got) {
		t.Fatalf("contained result escaped base: %s", got)
	}
	// And it must accept even when the base itself is reached through a symlink
	// (macOS /var -> /private/var): resolving an existing nested dir stays in base.
	if err := os.MkdirAll(got, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Contain(base, filepath.Join("res", "db", "leaf")); err != nil {
		t.Fatalf("Contain must accept a contained path under an existing (possibly symlinked) base: %v", err)
	}
}

func TestDisjointDetectsNesting(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if !disjoint(a, b) {
		t.Fatal("sibling boundary roots must be disjoint")
	}
	if disjoint(a, filepath.Join(a, "child")) {
		t.Fatal("a parent and its child are NOT disjoint (the child is reachable by walking)")
	}
	if disjoint(a, a) {
		t.Fatal("identical paths are not disjoint")
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
