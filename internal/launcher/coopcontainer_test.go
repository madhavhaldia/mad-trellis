package launcher

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitInitRepo makes dir a git repo so coopwiring's per-worktree exclude resolves —
// the precondition for the trunk-pollution guard to actually exclude (rather than
// remove) what Wire writes. Mirrors coopwiring_test.gitInit.
func gitInitRepo(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

// TestStageMadSubstrate stages the embedded linux mad-substrate binary into a scratch dir at
// the known in-container name, executable, with byte-identical content, and returns a
// path UNDER the scratch (which equals the in-container path).
func TestStageMadSubstrate(t *testing.T) {
	prev := madSubstrateBytesFn
	want := []byte("EMBEDDED-LINUX-mad-substrate")
	madSubstrateBytesFn = func(arch string) ([]byte, bool) {
		if arch != "arm64" {
			return nil, false
		}
		return want, true
	}
	t.Cleanup(func() { madSubstrateBytesFn = prev })

	scratch := t.TempDir()
	path, err := stageMadSubstrate(scratch, "arm64")
	if err != nil {
		t.Fatalf("stageMadSubstrate: %v", err)
	}
	if got := filepath.Join(scratch, madSubstrateStageName); path != got {
		t.Fatalf("staged path = %q; want %q (under scratch, == in-container path)", path, got)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read staged mad-substrate: %v", rerr)
	}
	if string(got) != string(want) {
		t.Fatalf("staged bytes = %q; want %q", got, want)
	}
	fi, serr := os.Stat(path)
	if serr != nil {
		t.Fatalf("stat staged mad-substrate: %v", serr)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("staged mad-substrate must be executable, mode=%v", fi.Mode())
	}
}

// TestStageMadSubstrate_NoEmbedFails is the NON-VACUOUS control for staging: with NO
// embedded payload (the untagged stub build), staging errors and writes NOTHING — the
// fail-soft path the caller logs and proceeds confined-without-MCP on. This proves
// TestStageMadSubstrate's success is due to the injected payload, not an unconditional write.
func TestStageMadSubstrate_NoEmbedFails(t *testing.T) {
	prev := madSubstrateBytesFn
	madSubstrateBytesFn = func(string) ([]byte, bool) { return nil, false }
	t.Cleanup(func() { madSubstrateBytesFn = prev })

	scratch := t.TempDir()
	path, err := stageMadSubstrate(scratch, "arm64")
	if err == nil {
		t.Fatalf("stageMadSubstrate must error when no payload is embedded; got path %q", path)
	}
	if path != "" {
		t.Fatalf("stageMadSubstrate must return no path on failure; got %q", path)
	}
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Fatalf("stageMadSubstrate must write nothing on failure; scratch has %d entries", len(entries))
	}
}

// TestStageMadSubstrate_NoScratchFails proves an empty scratch dir is a hard error (no
// staging surface), distinct from the no-embed control above.
func TestStageMadSubstrate_NoScratchFails(t *testing.T) {
	prev := madSubstrateBytesFn
	madSubstrateBytesFn = func(string) ([]byte, bool) { return []byte("x"), true }
	t.Cleanup(func() { madSubstrateBytesFn = prev })
	if _, err := stageMadSubstrate("  ", "arm64"); err == nil {
		t.Fatal("stageMadSubstrate must error with an empty scratch dir")
	}
}

// TestWireContainerMCP_Claude proves the in-container wiring writes Claude's .mcp.json
// INTO the in-container clone (hostWorktree, == /work) with the MCP `command` pointed at
// the STAGED in-container mad-substrate path (under scratch) + arg "mcp", and git-EXCLUDES
// it so it can never reach the validated trunk.
func TestWireContainerMCP_Claude(t *testing.T) {
	prev := madSubstrateBytesFn
	madSubstrateBytesFn = func(string) ([]byte, bool) { return []byte("LINUX-NM"), true }
	t.Cleanup(func() { madSubstrateBytesFn = prev })

	clone := t.TempDir()
	gitInitRepo(t, clone)
	scratch := t.TempDir()

	res, err := wireContainerMCP("claude", clone, scratch, "arm64", nil)
	if err != nil {
		t.Fatalf("wireContainerMCP: %v", err)
	}

	stagedPath := filepath.Join(scratch, madSubstrateStageName)

	// .mcp.json lands in the CLONE (mounted at /work), not the scratch.
	mcpPath := filepath.Join(clone, ".mcp.json")
	b, rerr := os.ReadFile(mcpPath)
	if rerr != nil {
		t.Fatalf("read .mcp.json from clone: %v", rerr)
	}
	var mcp map[string]any
	if uerr := json.Unmarshal(b, &mcp); uerr != nil {
		t.Fatalf("unmarshal .mcp.json: %v\n%s", uerr, b)
	}
	servers, _ := mcp["mcpServers"].(map[string]any)
	nm, _ := servers["mad-substrate"].(map[string]any)
	if nm == nil {
		t.Fatalf(".mcp.json missing mcpServers.mad-substrate: %#v", mcp)
	}
	// CRITICAL: command must be the STAGED IN-CONTAINER path, not a host binary.
	if got := nm["command"]; got != stagedPath {
		t.Fatalf(".mcp.json command = %v; want STAGED in-container path %q", got, stagedPath)
	}
	if !strings.HasPrefix(stagedPath, scratch) {
		t.Fatalf("staged path %q must be under the container scratch %q", stagedPath, scratch)
	}
	args, _ := nm["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Fatalf(`.mcp.json args = %#v; want ["mcp"]`, nm["args"])
	}

	// Trunk-pollution guard: .mcp.json must be git-excluded in the clone.
	if !contains(res.Excluded, ".mcp.json") {
		t.Fatalf("Result.Excluded must contain .mcp.json; got %#v", res.Excluded)
	}
	excPath := filepath.Join(clone, ".git", "info", "exclude")
	exc, _ := os.ReadFile(excPath)
	if !linePresent(string(exc), ".mcp.json") {
		t.Fatalf("git exclude must list .mcp.json so it cannot reach trunk; contents:\n%s", exc)
	}
}

// TestWireContainerMCP_Codex proves Codex is wired via `-c` ExtraArgs that reference the
// STAGED in-container mad-substrate path (it writes no on-disk files).
func TestWireContainerMCP_Codex(t *testing.T) {
	prev := madSubstrateBytesFn
	madSubstrateBytesFn = func(string) ([]byte, bool) { return []byte("LINUX-NM"), true }
	t.Cleanup(func() { madSubstrateBytesFn = prev })

	clone := t.TempDir()
	gitInitRepo(t, clone)
	scratch := t.TempDir()

	res, err := wireContainerMCP("codex", clone, scratch, "arm64", nil)
	if err != nil {
		t.Fatalf("wireContainerMCP codex: %v", err)
	}
	stagedPath := filepath.Join(scratch, madSubstrateStageName)
	joined := strings.Join(res.ExtraArgs, " ")
	if !strings.Contains(joined, `mcp_servers.mad-substrate.command="`+stagedPath+`"`) {
		t.Fatalf("codex ExtraArgs must point command at the staged in-container path %q; got %#v", stagedPath, res.ExtraArgs)
	}
	if !strings.Contains(joined, `mcp_servers.mad-substrate.args=["mcp"]`) {
		t.Fatalf(`codex ExtraArgs must set args=["mcp"]; got %#v`, res.ExtraArgs)
	}
	// Codex writes no files into the clone.
	if _, statErr := os.Stat(filepath.Join(clone, ".mcp.json")); !os.IsNotExist(statErr) {
		t.Fatalf("codex must NOT write .mcp.json into the clone")
	}
}

// TestWireContainerMCP_NoEmbedFailSoft is the NON-VACUOUS control for the wiring path:
// when no mad-substrate binary is embedded (the untagged build), wireContainerMCP returns an
// error AND writes NO config into the clone — so the launcher's (2e) fails soft (logs,
// runs the agent confined-uncooperative) rather than wiring a command to a non-existent
// staged binary. This proves the success tests above depend on the injected payload.
func TestWireContainerMCP_NoEmbedFailSoft(t *testing.T) {
	prev := madSubstrateBytesFn
	madSubstrateBytesFn = func(string) ([]byte, bool) { return nil, false }
	t.Cleanup(func() { madSubstrateBytesFn = prev })

	clone := t.TempDir()
	gitInitRepo(t, clone)
	scratch := t.TempDir()

	res, err := wireContainerMCP("claude", clone, scratch, "arm64", nil)
	if err == nil {
		t.Fatalf("wireContainerMCP must error when no mad-substrate is embedded; got %#v", res)
	}
	if _, statErr := os.Stat(filepath.Join(clone, ".mcp.json")); !os.IsNotExist(statErr) {
		t.Fatalf("no config must be written into the clone when staging fails")
	}
}

// TestWireContainerMCP_NoClone proves an empty hostWorktree (no real clone) is a hard
// error before any staging — there is nowhere to wire.
func TestWireContainerMCP_NoClone(t *testing.T) {
	prev := madSubstrateBytesFn
	madSubstrateBytesFn = func(string) ([]byte, bool) { return []byte("x"), true }
	t.Cleanup(func() { madSubstrateBytesFn = prev })
	if _, err := wireContainerMCP("claude", "  ", t.TempDir(), "arm64", nil); err == nil {
		t.Fatal("wireContainerMCP must error with an empty in-container clone path")
	}
}

// contains reports whether xs contains s.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// linePresent reports whether body has a line equal to want (trimmed).
func linePresent(body, want string) bool {
	for _, ln := range strings.Split(body, "\n") {
		if strings.TrimSpace(ln) == want {
			return true
		}
	}
	return false
}
