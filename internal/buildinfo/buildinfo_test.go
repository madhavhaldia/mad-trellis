package buildinfo

import (
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.38.0", "2.40.1", -1},
		{"2.40.1", "2.38.0", 1},
		{"2.38.0", "2.38.0", 0},
		{"2.38", "2.38.0", 0},                // missing trailing component == 0
		{"2.38.0", "2.38", 0},                // symmetric
		{"2.39.5-rc1", "2.39.5", 0},          // suffix on a component ignored
		{"v2.38.0", "2.38.0", 0},             // leading v stripped
		{"2.39", "2.38.99", 1},               // minor dominates
		{"10.0.0", "9.99.99", 1},             // numeric, not lexical
		{"2.9.0", "2.38.0", -1},              // git-floor case: 2.9 < 2.38 numerically (lexical would invert)
		{"2.38.0", "2.9.0", 1},               // symmetric to the git-floor case
		{"1.2.3.4", "1.2.3", 1},              // extra component
		{"", "0.0.0", 0},                     // empty == zero
		{"2.34.1 (Apple Git)", "2.38.0", -1}, // only leading int per component
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLoadManifest(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Go == "" {
		t.Error("manifest Go pin empty")
	}
	if len(m.Platforms) == 0 {
		t.Error("manifest Platforms empty")
	}
	git, ok := m.ConductedTools["git"]
	if !ok {
		t.Fatal("manifest missing conducted_tools.git")
	}
	if git.Min != "2.38.0" {
		t.Errorf("git.Min = %q, want 2.38.0 (load-bearing merge-tree --write-tree floor)", git.Min)
	}
}

func TestRenderNonVerbose(t *testing.T) {
	got := Render("1.2.3", "abc1234", 7, false)
	want := "mad-trellis 1.2.3 (commit abc1234, contract v7)\n"
	if got != want {
		t.Errorf("non-verbose Render = %q, want %q", got, want)
	}
}

func TestRenderVerbose(t *testing.T) {
	got := Render("1.2.3", "abc1234", 7, true)
	// Verbose must still START with the exact canonical line.
	first := "mad-trellis 1.2.3 (commit abc1234, contract v7)\n"
	if !strings.HasPrefix(got, first) {
		t.Errorf("verbose Render must start with canonical line; got:\n%s", got)
	}
	// And it must add the pin detail.
	for _, sub := range []string{"go:", "platforms:", "git min:"} {
		if !strings.Contains(got, sub) {
			t.Errorf("verbose Render missing %q; got:\n%s", sub, got)
		}
	}
	if !strings.Contains(got, "2.38.0") {
		t.Errorf("verbose Render missing git min 2.38.0; got:\n%s", got)
	}
}

func TestCheckGit(t *testing.T) {
	// Floor of 0.0.0 must pass against any installed git; also exercises parsing.
	have, ok, err := CheckGit("0.0.0")
	if err != nil {
		t.Skipf("git unavailable in test env: %v", err)
	}
	if !ok {
		t.Errorf("CheckGit(0.0.0) ok=false (have %q)", have)
	}
	if have == "" {
		t.Error("CheckGit returned empty have version")
	}
	// An impossibly high floor must fail-closed (ok=false, no error).
	_, ok2, err2 := CheckGit("999.0.0")
	if err2 != nil {
		t.Fatalf("CheckGit(999.0.0): unexpected err %v", err2)
	}
	if ok2 {
		t.Error("CheckGit(999.0.0) ok=true; expected fail-closed")
	}
}
