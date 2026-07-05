package main

import "testing"

// TestExitWithParent exhaustively pins the env gate that opts a daemon into
// self-terminating when its parent dies. Only scratch/test daemons (booted by
// `conform`) set MAD_EXIT_WITH_PARENT; production daemons leave it unset
// and must therefore be unaffected (empty → false).
func TestExitWithParent(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		// Truthy spellings (case- and whitespace-insensitive).
		{"1", true},
		{"true", true},
		{"on", true},
		{"yes", true},
		{" TRUE ", true},
		{"YES", true},
		{"On", true},

		// Falsey / production default: unset, explicit off, garbage.
		{"", false},
		{"0", false},
		{"off", false},
		{"false", false},
		{"no", false},
		{"garbage", false},
		{"2", false},
		{"  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("MAD_EXIT_WITH_PARENT", tc.val)
			if got := exitWithParent(); got != tc.want {
				t.Fatalf("exitWithParent() with %q = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
