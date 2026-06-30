package main

import "testing"

// TestEffectiveGrainIsContainer covers the requested-grain signal the launcher uses
// to decide pass-through vs host-resolution (and to fail-closed on a mismatch).
func TestEffectiveGrainIsContainer(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"container", true},
		{"Container", true},
		{"  container  ", true},
		{"worktree", false},
		{"", false},
	}
	for _, c := range cases {
		t.Setenv("MAD_GRAIN", c.val)
		if got := effectiveGrainIsContainer(); got != c.want {
			t.Errorf("MAD_GRAIN=%q: got %v want %v", c.val, got, c.want)
		}
	}
}
