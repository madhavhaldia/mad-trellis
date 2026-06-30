package coopwiring

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testBin = "/usr/local/bin/mad-substrate"

// gitInit makes dir a git repo so the per-worktree exclude path resolves. Tests
// that want the non-git path simply skip this.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

// readJSON reads path as a generic JSON object for assertions.
func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", path, err, b)
	}
	return m
}

// excludeContents returns the per-worktree exclude file contents (or "").
func excludeContents(t *testing.T, worktreeDir string) string {
	t.Helper()
	p, err := worktreeExcludePath(worktreeDir)
	if err != nil {
		t.Fatalf("resolve exclude path: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read exclude: %v", err)
	}
	return string(b)
}

// firstHookCommand digs out the inner "command" of the first entry in a host
// hook-event array.
func firstHookCommand(t *testing.T, hooks map[string]any, event string) string {
	t.Helper()
	arr, ok := hooks[event].([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("event %q missing or empty: %#v", event, hooks[event])
	}
	entry, ok := arr[0].(map[string]any)
	if !ok {
		t.Fatalf("event %q entry 0 not an object: %#v", event, arr[0])
	}
	inner, ok := entry["hooks"].([]any)
	if !ok || len(inner) == 0 {
		t.Fatalf("event %q entry 0 has no hooks: %#v", event, entry)
	}
	hm, ok := inner[0].(map[string]any)
	if !ok {
		t.Fatalf("event %q inner 0 not an object: %#v", event, inner[0])
	}
	cmd, _ := hm["command"].(string)
	return cmd
}

func TestBinaryPath(t *testing.T) {
	p, err := BinaryPath()
	if err != nil {
		t.Fatalf("BinaryPath: %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Errorf("BinaryPath not absolute: %q", p)
	}
	// It must resolve to a real existing file (the test binary itself).
	if _, err := os.Stat(p); err != nil {
		t.Errorf("BinaryPath %q does not stat: %v", p, err)
	}
}

func TestWireClaude(t *testing.T) {
	wt := t.TempDir()
	gitInit(t, wt)

	res, err := Wire("claude", wt, testBin)
	if err != nil {
		t.Fatalf("Wire claude: %v", err)
	}

	// ExtraArgs is empty for Claude.
	if len(res.ExtraArgs) != 0 {
		t.Errorf("claude ExtraArgs should be empty, got %#v", res.ExtraArgs)
	}

	// .mcp.json: command == binPath, args == ["mcp"].
	mcp := readJSONFile(t, filepath.Join(wt, ".mcp.json"))
	servers, _ := mcp["mcpServers"].(map[string]any)
	nm, _ := servers["mad-substrate"].(map[string]any)
	if nm == nil {
		t.Fatalf(".mcp.json missing mcpServers.mad-substrate: %#v", mcp)
	}
	if got := nm["command"]; got != testBin {
		t.Errorf(".mcp.json command: want %q, got %v", testBin, got)
	}
	args, _ := nm["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Errorf(`.mcp.json args: want ["mcp"], got %#v`, nm["args"])
	}

	// settings.local.json: enableAllProjectMcpServers + ONLY the SessionStart hook.
	settings := readJSONFile(t, filepath.Join(wt, ".claude", "settings.local.json"))
	if settings["enableAllProjectMcpServers"] != true {
		t.Errorf("enableAllProjectMcpServers: want true, got %v", settings["enableAllProjectMcpServers"])
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatalf("settings.local.json missing hooks: %#v", settings)
	}
	if got, want := firstHookCommand(t, hooks, "SessionStart"), q(testBin)+" hook claude-sessionstart"; got != want {
		t.Errorf("SessionStart command: want %q, got %q", want, got)
	}
	// The removed per-edit / lifecycle hooks must NOT be present.
	for _, ev := range []string{"PreToolUse", "SessionEnd"} {
		if _, present := hooks[ev]; present {
			t.Errorf("hook %s must NOT be wired anymore: %#v", ev, hooks[ev])
		}
	}

	// Wrote + Excluded carry the relative paths.
	assertContains(t, "Wrote", res.Wrote, ".mcp.json", ".claude/settings.local.json")
	assertContains(t, "Excluded", res.Excluded, ".mcp.json", ".claude/settings.local.json")

	// Exclude file contains both patterns.
	exc := excludeContents(t, wt)
	for _, p := range []string{".mcp.json", ".claude/settings.local.json"} {
		if !lineIn(exc, p) {
			t.Errorf("exclude missing %q; contents:\n%s", p, exc)
		}
	}
}

func TestWireClaudeMergeAndIdempotency(t *testing.T) {
	wt := t.TempDir()
	gitInit(t, wt)

	// Pre-existing user .mcp.json must NOT be touched (no clobber).
	userMCP := `{"mcpServers":{"other":{"command":"other-bin","args":["x"]}}}`
	if err := os.WriteFile(filepath.Join(wt, ".mcp.json"), []byte(userMCP), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-existing settings with a user-defined hook we must preserve.
	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	userSettings := map[string]any{
		"someUserKey": "keepme",
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": "my-own-hook",
				}},
			}},
		},
	}
	sb, _ := json.MarshalIndent(userSettings, "", "  ")
	if err := os.WriteFile(filepath.Join(wt, ".claude", "settings.local.json"), sb, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Wire("claude", wt, testBin)
	if err != nil {
		t.Fatalf("Wire claude: %v", err)
	}

	// .mcp.json NOT modified (still the user's content) and NOT in Wrote.
	gotMCP, _ := os.ReadFile(filepath.Join(wt, ".mcp.json"))
	if strings.TrimSpace(string(gotMCP)) != userMCP {
		t.Errorf(".mcp.json was modified; want %q, got %q", userMCP, gotMCP)
	}
	for _, w := range res.Wrote {
		if w == ".mcp.json" {
			t.Errorf(".mcp.json should be skipped (pre-existing) but is in Wrote: %#v", res.Wrote)
		}
	}

	// settings: user key preserved.
	settings := readJSONFile(t, filepath.Join(wt, ".claude", "settings.local.json"))
	if settings["someUserKey"] != "keepme" {
		t.Errorf("user key dropped: %#v", settings)
	}
	if settings["enableAllProjectMcpServers"] != true {
		t.Errorf("enableAllProjectMcpServers not set: %v", settings["enableAllProjectMcpServers"])
	}
	hooks, _ := settings["hooks"].(map[string]any)
	ss, _ := hooks["SessionStart"].([]any)
	// user hook + our hook = 2 SessionStart entries.
	if len(ss) != 2 {
		t.Fatalf("SessionStart should have user + ours = 2, got %d: %#v", len(ss), ss)
	}
	// user hook preserved.
	if !preToolHasCommand(ss, "my-own-hook") {
		t.Errorf("user hook 'my-own-hook' was dropped: %#v", ss)
	}
	// our hook appended.
	if !preToolHasCommand(ss, q(testBin)+" hook claude-sessionstart") {
		t.Errorf("our SessionStart hook missing: %#v", ss)
	}

	// SECOND Wire must NOT duplicate our hook entry.
	if _, err := Wire("claude", wt, testBin); err != nil {
		t.Fatalf("second Wire: %v", err)
	}
	settings2 := readJSONFile(t, filepath.Join(wt, ".claude", "settings.local.json"))
	hooks2, _ := settings2["hooks"].(map[string]any)
	ss2, _ := hooks2["SessionStart"].([]any)
	if len(ss2) != 2 {
		t.Errorf("second Wire duplicated SessionStart: want 2, got %d: %#v", len(ss2), ss2)
	}

	// Exclude file should not have duplicated the settings pattern either.
	exc := excludeContents(t, wt)
	if n := strings.Count(exc, ".claude/settings.local.json"); n != 1 {
		t.Errorf("exclude pattern duplicated: count=%d\n%s", n, exc)
	}
}

func TestWireCodex(t *testing.T) {
	wt := t.TempDir()
	gitInit(t, wt)

	res, err := Wire("codex", wt, testBin)
	if err != nil {
		t.Fatalf("Wire codex: %v", err)
	}

	// ExtraArgs: ONLY the two -c pairs (no --dangerously-bypass-hook-trust).
	wantArgs := []string{
		"-c", `mcp_servers.mad-substrate.command="` + testBin + `"`,
		"-c", `mcp_servers.mad-substrate.args=["mcp"]`,
	}
	if len(res.ExtraArgs) != len(wantArgs) {
		t.Fatalf("ExtraArgs len: want %d, got %d (%#v)", len(wantArgs), len(res.ExtraArgs), res.ExtraArgs)
	}
	for i := range wantArgs {
		if res.ExtraArgs[i] != wantArgs[i] {
			t.Errorf("ExtraArgs[%d]: want %q, got %q", i, wantArgs[i], res.ExtraArgs[i])
		}
	}

	// Codex now writes NO files: no .codex/ directory at all, nothing Wrote/Excluded.
	if _, statErr := os.Stat(filepath.Join(wt, ".codex")); !os.IsNotExist(statErr) {
		t.Errorf(".codex must not be written for codex, stat err=%v", statErr)
	}
	if len(res.Wrote) != 0 || len(res.Excluded) != 0 {
		t.Errorf("codex must write nothing: Wrote=%#v Excluded=%#v", res.Wrote, res.Excluded)
	}
}

func TestWireUnknownHost(t *testing.T) {
	wt := t.TempDir()
	res, err := Wire("emacs", wt, testBin)
	if err != nil {
		t.Errorf("unknown host err: want nil, got %v", err)
	}
	if len(res.ExtraArgs) != 0 || len(res.Wrote) != 0 || len(res.Excluded) != 0 {
		t.Errorf("unknown host should yield zero Result, got %#v", res)
	}
	// Nothing written.
	if entries, _ := os.ReadDir(wt); len(entries) != 0 {
		t.Errorf("unknown host wrote files: %#v", entries)
	}
}

func TestWireNonGitDirStillWritesFiles(t *testing.T) {
	// A non-git worktreeDir: files MUST still be written, exclude is skipped, no
	// panic.
	wt := t.TempDir() // deliberately NOT git init'd

	res, err := Wire("claude", wt, testBin)
	// addExclude fails (not a git repo); Wire surfaces that as the first error but
	// must still have written the files.
	if err == nil {
		t.Logf("non-git Wire returned nil error (acceptable)")
	}
	if _, statErr := os.Stat(filepath.Join(wt, ".mcp.json")); statErr != nil {
		t.Errorf(".mcp.json not written in non-git dir: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(wt, ".claude", "settings.local.json")); statErr != nil {
		t.Errorf("settings.local.json not written in non-git dir: %v", statErr)
	}
	// Files were written so Wrote is populated even though excluding failed.
	assertContains(t, "Wrote", res.Wrote, ".mcp.json", ".claude/settings.local.json")

	// Codex writes NO files (git or not); only ExtraArgs is populated.
	wt2 := t.TempDir()
	res2, _ := Wire("codex", wt2, testBin)
	if _, statErr := os.Stat(filepath.Join(wt2, ".codex")); !os.IsNotExist(statErr) {
		t.Errorf("codex must not write .codex even in non-git dir, stat err=%v", statErr)
	}
	if len(res2.ExtraArgs) == 0 {
		t.Errorf("codex ExtraArgs should be populated even in non-git dir")
	}
}

func TestWireClaudeCorruptSettingsNotClobbered(t *testing.T) {
	// A pre-existing but CORRUPT settings.local.json must be left UNTOUCHED — we
	// never destroy a user's config (it may be momentarily unparseable), mirroring
	// the .mcp.json skip-if-present discipline. The independent .mcp.json is still
	// written.
	wt := t.TempDir()
	gitInit(t, wt)
	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{ this is not valid json ")
	settingsPath := filepath.Join(wt, ".claude", "settings.local.json")
	if err := os.WriteFile(settingsPath, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Wire("claude", wt, testBin)
	if err == nil {
		t.Errorf("Wire should surface the corrupt-settings read error")
	}
	// The corrupt file is preserved byte-for-byte (NOT overwritten).
	got, rerr := os.ReadFile(settingsPath)
	if rerr != nil {
		t.Fatalf("read settings: %v", rerr)
	}
	if string(got) != string(corrupt) {
		t.Errorf("corrupt settings.local.json was modified:\n got %q\nwant %q", got, corrupt)
	}
	// settings.local.json must NOT be in Wrote (we skipped it).
	for _, w := range res.Wrote {
		if w == ".claude/settings.local.json" {
			t.Errorf("settings.local.json should not be in Wrote when skipped: %v", res.Wrote)
		}
	}
	// The independent .mcp.json was still written before the skip.
	if _, statErr := os.Stat(filepath.Join(wt, ".mcp.json")); statErr != nil {
		t.Errorf(".mcp.json should still be written: %v", statErr)
	}
}

// TestWireIntegratorClaude: the integrator variant writes the SAME claude config
// shape as Wire, EXCEPT the MCP args are ["mcp","--role","integrator"], plus a
// git-excluded guidance file.
func TestWireIntegratorClaude(t *testing.T) {
	wt := t.TempDir()
	gitInit(t, wt)

	res, err := WireIntegrator("claude", wt, testBin)
	if err != nil {
		t.Fatalf("WireIntegrator claude: %v", err)
	}
	if len(res.ExtraArgs) != 0 {
		t.Errorf("claude ExtraArgs should be empty, got %#v", res.ExtraArgs)
	}

	// .mcp.json args MUST be the role'd args.
	mcp := readJSONFile(t, filepath.Join(wt, ".mcp.json"))
	servers, _ := mcp["mcpServers"].(map[string]any)
	nm, _ := servers["mad-substrate"].(map[string]any)
	if nm == nil {
		t.Fatalf(".mcp.json missing mcpServers.mad-substrate: %#v", mcp)
	}
	if got := nm["command"]; got != testBin {
		t.Errorf(".mcp.json command: want %q, got %v", testBin, got)
	}
	args, _ := nm["args"].([]any)
	want := []any{"mcp", "--role", "integrator"}
	if len(args) != len(want) {
		t.Fatalf(`.mcp.json args: want ["mcp","--role","integrator"], got %#v`, nm["args"])
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf(".mcp.json args[%d]: want %v, got %v", i, want[i], args[i])
		}
	}

	// The SessionStart guidance hook is still wired (same shape as Wire).
	settings := readJSONFile(t, filepath.Join(wt, ".claude", "settings.local.json"))
	hooks, _ := settings["hooks"].(map[string]any)
	if got, want := firstHookCommand(t, hooks, "SessionStart"), q(testBin)+" hook claude-sessionstart"; got != want {
		t.Errorf("SessionStart command: want %q, got %q", want, got)
	}

	// Guidance markdown written + git-excluded.
	if _, statErr := os.Stat(filepath.Join(wt, integratorGuideFile)); statErr != nil {
		t.Errorf("integrator guidance %q not written: %v", integratorGuideFile, statErr)
	}
	assertContains(t, "Wrote", res.Wrote, ".mcp.json", ".claude/settings.local.json", integratorGuideFile)
	assertContains(t, "Excluded", res.Excluded, ".mcp.json", ".claude/settings.local.json", integratorGuideFile)

	exc := excludeContents(t, wt)
	for _, p := range []string{".mcp.json", ".claude/settings.local.json", integratorGuideFile} {
		if !lineIn(exc, p) {
			t.Errorf("exclude missing %q; contents:\n%s", p, exc)
		}
	}
}

// TestWireIntegratorCodex: the integrator variant's codex ExtraArgs carry the
// role'd args array literal; the guidance file is still written + excluded.
func TestWireIntegratorCodex(t *testing.T) {
	wt := t.TempDir()
	gitInit(t, wt)

	res, err := WireIntegrator("codex", wt, testBin)
	if err != nil {
		t.Fatalf("WireIntegrator codex: %v", err)
	}
	wantArgs := []string{
		"-c", `mcp_servers.mad-substrate.command="` + testBin + `"`,
		"-c", `mcp_servers.mad-substrate.args=["mcp","--role","integrator"]`,
	}
	if len(res.ExtraArgs) != len(wantArgs) {
		t.Fatalf("ExtraArgs len: want %d, got %d (%#v)", len(wantArgs), len(res.ExtraArgs), res.ExtraArgs)
	}
	for i := range wantArgs {
		if res.ExtraArgs[i] != wantArgs[i] {
			t.Errorf("ExtraArgs[%d]: want %q, got %q", i, wantArgs[i], res.ExtraArgs[i])
		}
	}

	// Guidance file is written + excluded even for codex (which writes no MCP file).
	if _, statErr := os.Stat(filepath.Join(wt, integratorGuideFile)); statErr != nil {
		t.Errorf("integrator guidance not written for codex: %v", statErr)
	}
	assertContains(t, "Wrote", res.Wrote, integratorGuideFile)
	assertContains(t, "Excluded", res.Excluded, integratorGuideFile)
	if !lineIn(excludeContents(t, wt), integratorGuideFile) {
		t.Errorf("guidance not git-excluded for codex")
	}
}

func TestQuoting(t *testing.T) {
	if got := q("/a b/null mutex"); got != `"/a b/null mutex"` {
		t.Errorf("q: got %q", got)
	}
}

func TestExcludeNoDuplicateAndNewlineSafety(t *testing.T) {
	wt := t.TempDir()
	gitInit(t, wt)

	// Seed the exclude file with a line that does NOT end in a newline, to verify
	// we insert a separator before appending.
	excPath, err := worktreeExcludePath(wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(excPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excPath, []byte("existing-pattern"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := addExclude(wt, ".mcp.json"); err != nil {
		t.Fatalf("addExclude: %v", err)
	}
	exc := excludeContents(t, wt)
	if !lineIn(exc, "existing-pattern") {
		t.Errorf("seeded pattern lost: %q", exc)
	}
	if !lineIn(exc, ".mcp.json") {
		t.Errorf("new pattern not on its own line: %q", exc)
	}

	// Re-add is a no-op (no duplicate).
	if err := addExclude(wt, ".mcp.json"); err != nil {
		t.Fatalf("addExclude (2): %v", err)
	}
	exc2 := excludeContents(t, wt)
	if n := strings.Count(exc2, ".mcp.json"); n != 1 {
		t.Errorf("duplicate exclude line: count=%d\n%s", n, exc2)
	}
}

// ---- small assertion helpers ----

func assertContains(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("%s missing %q; got %#v", label, w, got)
		}
	}
}

func lineIn(contents, pattern string) bool {
	for _, line := range strings.Split(contents, "\n") {
		if strings.TrimSpace(line) == pattern {
			return true
		}
	}
	return false
}

func preToolHasCommand(arr []any, cmd string) bool {
	return hookArrayHasCommand(arr, cmd)
}
