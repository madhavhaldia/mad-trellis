package substrate

import (
	"path/filepath"
	"testing"
)

// Two checkouts of ONE repo (a main checkout and a LINKED git worktree) must
// share the SAME stateBase — per-agent state is keyed on the canonical git
// identity, not the per-checkout path. Without this, a daemon restart from a
// different checkout derives a different base and liveness reclaim cannot find
// the old state dir (Inv 3 leak).
func TestStateBaseSharedAcrossCheckouts(t *testing.T) {
	t.Setenv("MAD_STATE_DIR", t.TempDir())
	repo := initRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitRun(t, repo, "worktree", "add", "-b", "wt-test", linked, "HEAD")

	mainAbs, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	linkedAbs, err := filepath.Abs(linked)
	if err != nil {
		t.Fatal(err)
	}
	if stateBase(mainAbs) != stateBase(linkedAbs) {
		t.Fatalf("two checkouts must share stateBase: main=%q linked=%q",
			stateBase(mainAbs), stateBase(linkedAbs))
	}
}

func TestStateRoot(t *testing.T) {
	t.Setenv("MAD_STATE_DIR", "/tmp/nmstate")
	got := StateRoot("/repo", "s-1-abc")
	if got == "" {
		t.Fatal("a valid slug must resolve")
	}
	// must equal the SAME derivation provisionState uses (stateBase/slug).
	want := filepath.Join(stateBase("/repo"), "s-1-abc")
	if got != want {
		t.Fatalf("StateRoot = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "../etc", "a/b", `a\b`, "x..y", ".."} {
		if StateRoot("/repo", bad) != "" {
			t.Errorf("unsafe slug %q must yield \"\"", bad)
		}
	}
}
