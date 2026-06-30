// Package coopwiring generates "cooperative-by-default" config for a launched
// agent so that `mad-substrate launch -- claude` / `-- codex` comes up already wired
// to the mad-substrate MCP server (and, for Claude, the SessionStart standing-guidance
// hook) with NO manual setup. It writes the host's config files into the agent's
// DISPOSABLE per-agent git worktree (the "boundary") and returns argv flags the
// launcher should inject.
//
// There is NO per-edit "claim-before-edit" hook: the cooperative layer is the
// MCP tools (which the agent calls to coordinate) plus standing guidance (which
// tells it to use them). Claude gets the SessionStart guidance hook on disk;
// Codex gets MCP via `-c` overrides and its guidance from the MCP server's
// instructions — it has NO files written.
//
// WHY config-on-disk in the boundary worktree (honor this design):
//   - The agent's MCP server and the Claude SessionStart hook are served by the
//     SAME mad-substrate binary via the `mad-substrate mcp` and `mad-substrate hook <event>`
//     subcommands. We only ever REFERENCE those subcommand names + the binary path
//     as strings here; this package imports neither internal/mcp, internal/coophook,
//     nor internal/coopclient — it is purely a config writer.
//   - We cannot relocate CLAUDE_CONFIG_DIR / CODEX_HOME: those dirs hold the
//     host's auth, so a fresh dir would yield an UNAUTHENTICATED agent. Claude has
//     no per-invocation MCP/hook flag, so its wiring MUST live on disk. Codex
//     takes MCP via `-c` overrides, so it needs no on-disk config at all.
//   - The disposable, per-agent boundary worktree (despawned later) is the correct
//     place for that on-disk config — NOT the user's main repo.
//
// CRITICAL trunk-pollution guard: anything we write into the worktree MUST be
// added to the worktree's git exclude. The agent commits its work and that commit
// is integrated to the trunk; an un-excluded `.mcp.json` (or `.claude/`,
// `.codex/`) would pollute the validated trunk — the very thing the substrate
// exists to prevent. The exclude is resolved via `git -C <wt> rev-parse
// --git-path info/exclude`; for a LINKED worktree that is the repo's SHARED
// (common) info/exclude — git has no per-worktree exclude — which still applies to
// the linked worktree, so the generated file is correctly ignored there and can
// never be committed. To hold the invariant even when the exclude cannot be
// written in a real repo, Wire REMOVES the file rather than leave it committable.
//
// FAIL-SOFT: wiring is a convenience, not a safety boundary. Wire is best-effort
// and returns the FIRST hard error for the caller to LOG; the caller still
// launches the agent if wiring fails.
package coopwiring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Result is the outcome of one Wire call.
type Result struct {
	// ExtraArgs are argv flags to PREPEND to the agent's own args (e.g. Codex
	// `-c` overrides). Empty for Claude, which is wired entirely on disk.
	ExtraArgs []string
	// Wrote lists the worktree-relative paths we created or modified.
	Wrote []string
	// Excluded lists the worktree-relative patterns we added to git exclude.
	Excluded []string
}

// BinaryPath returns the absolute, symlink-resolved path to the running mad-substrate
// binary (os.Executable then filepath.EvalSymlinks). When launched via the agent
// shim (argv0 is a "claude"/"codex" symlink), this must STILL resolve to the real
// mad-substrate binary that carries the mcp/hook subcommands — EvalSymlinks does that
// because os.Executable returns the actual executable image, not argv0.
func BinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// The image exists (os.Executable succeeded) but could not be resolved
		// through symlinks; fall back to the unresolved absolute path rather than
		// failing wiring entirely.
		if abs, aerr := filepath.Abs(exe); aerr == nil {
			return abs, nil
		}
		return exe, nil
	}
	return resolved, nil
}

// Wire writes cooperative config for host ("claude" | "codex") into the boundary
// worktree at worktreeDir, referencing binPath (the absolute mad-substrate path), and
// returns argv flags to inject. An unknown host yields a zero Result and nil
// error. It is best-effort but returns the FIRST hard error for the caller to log
// (the caller treats wiring as fail-soft and still launches the agent on error).
func Wire(host, worktreeDir, binPath string) (Result, error) {
	return wire(host, worktreeDir, binPath, defaultMCPArgs())
}

// defaultMCPArgs are the cooperative MCP server's subcommand args — plain
// `mad-substrate mcp` (the builder/cooperative role).
func defaultMCPArgs() []string { return []string{"mcp"} }

// integratorMCPArgs point the SAME on-disk config shape at `mad-substrate mcp
// --role integrator` (FROZEN CONTRACT): a sibling unit adds the `--role` flag
// and the integrator MCP server serves the integration toolset + acquires the
// singleton presence lease. coopwiring only references the subcommand+flag as
// strings; it imports none of the MCP layer.
func integratorMCPArgs() []string { return []string{"mcp", "--role", "integrator"} }

// wire is the shared implementation: it writes the cooperative on-disk config
// for host, but with the MCP server invoked as `binPath <mcpArgs...>`. Wire
// passes the default `mcp` args; WireIntegrator passes the `mcp --role
// integrator` args. An unknown host yields a zero Result and nil error.
func wire(host, worktreeDir, binPath string, mcpArgs []string) (Result, error) {
	switch host {
	case "claude":
		return wireClaude(worktreeDir, binPath, mcpArgs)
	case "codex":
		return wireCodex(worktreeDir, binPath, mcpArgs)
	default:
		// We never assume a host whose config schema we do not recognize.
		return Result{}, nil
	}
}

// WireIntegrator writes the SAME cooperative config shape as Wire into the trunk
// worktree at dir, EXCEPT the MCP server is invoked as `mad-substrate mcp --role
// integrator`, and it additionally drops a short, git-excluded integrator
// guidance file the agent can read. Unlike Wire's disposable boundary, dir is
// the user's actual trunk/feature worktree — so the trunk-pollution guard
// (git-exclude every generated file) is doubly load-bearing here. Best-effort /
// fail-soft: it returns the FIRST hard error for the caller to LOG, having still
// written what it could; the caller launches the integrator regardless.
func WireIntegrator(host, dir, binPath string) (Result, error) {
	res, firstErr := wire(host, dir, binPath, integratorMCPArgs())

	// Drop the standing integrator guidance markdown (git-excluded exactly like
	// every other generated file, via the shared exclude guard). This is advisory
	// for the agent and never reaches the trunk.
	guidePath := filepath.Join(dir, integratorGuideFile)
	if err := writeExcludedBytes(dir, guidePath, integratorGuideFile, []byte(integratorGuide), &res); err != nil && firstErr == nil {
		firstErr = err
	}
	return res, firstErr
}

// integratorGuideFile is the worktree-relative path of the standing guidance the
// integrator agent reads. Git-excluded so it can never be committed to trunk.
const integratorGuideFile = ".mad-substrate/integrator.md"

// integratorGuide is the short reference prompt for the integrator agent. It
// names the integrator MCP tools (served by `mad-substrate mcp --role integrator`,
// the FROZEN contract) without this package depending on the MCP layer.
const integratorGuide = `# You are the mad-substrate integrator

You run directly in the trunk worktree and merge builder branches into it, gated.
Exactly one integrator runs per trunk (a singleton presence lease enforces this).

Workflow:
- Use ` + "`mad_integration_pending`" + ` to list integration requests, then
  ` + "`mad_integration_claim`" + ` to take one.
- Review the claimed branch's diff against HEAD. Read the changes critically; run
  the build/tests and any extra checks the user asked for.
- If it is correct and safe, ` + "`mad_integration_approve`" + ` (a gated merge
  through the trunk lease — never merge by hand).
- If not, ` + "`mad_integration_reject`" + ` with specific, actionable feedback
  so the builder can fix it.

Never push to trunk directly; the gated approve is the only promotion path.
`

// q wraps p in double quotes. The hook command is a single shell string, so
// quoting the binary path tolerates spaces in it.
func q(p string) string { return `"` + p + `"` }

// ---- claude ----------------------------------------------------------------

const (
	claudeMCPFile     = ".mcp.json"
	claudeSettingsRel = ".claude/settings.local.json"
	claudeSettingsDir = ".claude"
)

// wireClaude writes Claude's on-disk MCP config + the SessionStart
// standing-guidance hook. mcpArgs is the MCP server's subcommand argv (e.g.
// ["mcp"] cooperative, or ["mcp","--role","integrator"]). ExtraArgs is empty:
// Claude has no per-invocation MCP/hook flag, so everything lives on disk.
func wireClaude(worktreeDir, binPath string, mcpArgs []string) (Result, error) {
	var res Result
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// 1) .mcp.json — only if it does NOT already exist. If the user already has an
	// .mcp.json we DO NOT clobber it (and skip MCP wiring for this run); the hooks
	// still get wired below.
	mcpPath := filepath.Join(worktreeDir, claudeMCPFile)
	if _, err := os.Stat(mcpPath); os.IsNotExist(err) {
		mcp := map[string]any{
			"mcpServers": map[string]any{
				"mad-substrate": map[string]any{
					"command": binPath,
					"args":    mcpArgs,
				},
			},
		}
		note(writeExcluded(worktreeDir, mcpPath, claudeMCPFile, mcp, &res))
	}

	// 2) .claude/settings.local.json — merge into any existing settings. A corrupt
	// existing file is left UNTOUCHED (we never clobber the user's config, mirroring
	// the .mcp.json skip-if-present discipline), at the cost of not wiring the hook
	// this run.
	if err := os.MkdirAll(filepath.Join(worktreeDir, claudeSettingsDir), 0o755); err != nil {
		note(err)
	}
	settingsPath := filepath.Join(worktreeDir, claudeSettingsRel)
	settings, rerr := readJSONObject(settingsPath)
	if rerr != nil {
		note(rerr) // unreadable/corrupt existing file: skip rather than overwrite it
		return res, firstErr
	}

	// Auto-approve the project MCP server so the agent never hits an interactive
	// approval prompt for it.
	settings["enableAllProjectMcpServers"] = true

	hooks := asObject(settings["hooks"])
	appendHook(hooks, "SessionStart", map[string]any{
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": q(binPath) + " hook claude-sessionstart",
		}},
	}, q(binPath)+" hook claude-sessionstart")
	settings["hooks"] = hooks

	note(writeExcluded(worktreeDir, settingsPath, claudeSettingsRel, settings, &res))

	// ExtraArgs is empty for Claude.
	return res, firstErr
}

// ---- codex -----------------------------------------------------------------

// wireCodex returns Codex's MCP `-c` overrides. Codex accepts MCP via
// per-invocation `-c` TOML overrides and gets its cooperative guidance from the
// MCP server's instructions, so it needs NO on-disk config — there is no per-edit
// hook to wire.
func wireCodex(_ /*worktreeDir*/ string, binPath string, mcpArgs []string) (Result, error) {
	var res Result

	// The args array is rendered as a TOML/JSON array literal — json.Marshal of a
	// []string yields exactly `["mcp"]` / `["mcp","--role","integrator"]` (no
	// spaces), the shape Codex's `-c` parser expects.
	argsLit, err := json.Marshal(mcpArgs)
	if err != nil {
		return Result{}, err
	}

	// MCP via argv `-c` overrides. These are argv ELEMENTS passed without a shell,
	// so the inner double-quotes are LITERAL characters that Codex's TOML-override
	// parser needs to read the value as a string / array.
	res.ExtraArgs = []string{
		"-c", `mcp_servers.mad-substrate.command="` + binPath + `"`,
		"-c", `mcp_servers.mad-substrate.args=` + string(argsLit),
	}

	return res, nil
}

// ---- shared JSON helpers ---------------------------------------------------

// readJSONObject reads path as a JSON object into a map. A missing file yields an
// empty map and no error (the common "create fresh" case). A present-but-corrupt
// file returns an error AND an empty map, so the caller starts clean rather than
// propagating garbage (fail-soft).
func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return map[string]any{}, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// writeJSON writes v as indented JSON to path, creating parent dirs as needed.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// asObject coerces v (typically from a decoded JSON map) into a map[string]any,
// returning a fresh empty map when v is absent or not an object. This lets us
// merge into existing config without dropping unrelated keys.
func asObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

// appendHook appends entry to the event array under hooks[event], creating the
// array if needed. IDEMPOTENT: if any existing entry already carries our exact
// hook command (wantCmd), we do NOT append — so a re-launch never stacks
// duplicate hook entries.
func appendHook(hooks map[string]any, event string, entry map[string]any, wantCmd string) {
	var arr []any
	if existing, ok := hooks[event].([]any); ok {
		arr = existing
		if hookArrayHasCommand(arr, wantCmd) {
			return // already wired; do not duplicate
		}
	}
	hooks[event] = append(arr, entry)
}

// hookArrayHasCommand reports whether any entry in a host hook-event array
// already contains an inner hook whose "command" equals wantCmd. It tolerates the
// nested {"hooks":[{"type":"command","command":...}]} shape both Claude and Codex
// use, as well as any user-authored entries already present.
func hookArrayHasCommand(arr []any, wantCmd string) bool {
	for _, e := range arr {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := entry["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, ok := hm["command"].(string); ok && cmd == wantCmd {
				return true
			}
		}
	}
	return false
}

// ---- git exclude (trunk-pollution guard) -----------------------------------

// errNoGitExclude signals that worktreeDir is not a git repo (or git is
// unavailable), so there is no exclude to write — and, equally, no commit can
// happen there, so an un-excluded generated file poses no trunk-pollution risk.
// It is distinct from a real exclude-write failure in a confirmed git repo, which
// IS dangerous (the file could be committed un-excluded).
var errNoGitExclude = errors.New("coopwiring: worktree is not a git repo; skipping exclude")

// writeExcluded writes v to absPath and guards it from the trunk by adding rel to
// the worktree's git exclude. The ordering is load-bearing: a generated file the
// agent could `git add` and commit would be integrated into the validated trunk,
// so in a REAL git repo where the exclude cannot be written we REMOVE the file
// again rather than leave it committable (fail-soft toward not-wiring over
// polluting). A non-git worktree keeps the file (no commit is possible there).
// Records Wrote/Excluded into res on success.
func writeExcluded(worktreeDir, absPath, rel string, v any, res *Result) error {
	if err := writeJSON(absPath, v); err != nil {
		return err
	}
	return guardExclude(worktreeDir, absPath, rel, res)
}

// writeExcludedBytes is writeExcluded for raw (non-JSON) content — e.g. the
// integrator guidance markdown. Same trunk-pollution guard.
func writeExcludedBytes(worktreeDir, absPath, rel string, content []byte, res *Result) error {
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(absPath, content, 0o644); err != nil {
		return err
	}
	return guardExclude(worktreeDir, absPath, rel, res)
}

// guardExclude adds rel to the worktree's git exclude after the file at absPath
// was written, and records Wrote/Excluded. The ordering is load-bearing: a
// generated file the agent could `git add` and commit would be integrated into
// the validated trunk, so in a REAL git repo where the exclude cannot be written
// we REMOVE the file again rather than leave it committable (fail-soft toward
// not-wiring over polluting). A non-git worktree keeps the file (no commit is
// possible there). Shared by writeExcluded (JSON) and writeExcludedBytes (raw).
func guardExclude(worktreeDir, absPath, rel string, res *Result) error {
	switch err := addExclude(worktreeDir, rel); {
	case err == nil:
		res.Wrote = append(res.Wrote, rel)
		res.Excluded = append(res.Excluded, rel)
		return nil
	case errors.Is(err, errNoGitExclude):
		// Not a git repo: the agent cannot commit here, so the file cannot reach
		// (and pollute) trunk. Keep it — wired but harmlessly un-excluded.
		res.Wrote = append(res.Wrote, rel)
		return nil
	default:
		// A real git repo whose exclude we could not write: keeping the file risks
		// it being committed and integrated. Remove it to preserve the invariant.
		_ = os.Remove(absPath)
		return err
	}
}

// addExclude appends pattern to the worktree's git exclude file so a file we
// generate is NEVER committed by the agent and therefore can never reach (and
// pollute) the validated trunk. For a LINKED worktree this resolves to the repo's
// SHARED (common) info/exclude — git has no per-worktree exclude — which still
// applies to the linked worktree, so the pattern correctly ignores the file there.
// De-duplication is best-effort: two concurrent launches into sibling worktrees of
// one repo race on the shared file and could append a duplicate line, which is
// harmless. Returns errNoGitExclude (wrapped) when worktreeDir is not a git repo /
// git is unavailable, so callers can tell "no exclude needed" from a real failure.
func addExclude(worktreeDir, pattern string) error {
	excludePath, err := worktreeExcludePath(worktreeDir)
	if err != nil {
		return fmt.Errorf("%w: %v", errNoGitExclude, err)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return err
	}
	if has, err := excludeHasPattern(excludePath, pattern); err != nil {
		return err
	} else if has {
		return nil // already excluded; do not duplicate the line
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Ensure the new pattern lands on its own line even if the file did not end
	// with a newline.
	if needsLeadingNewline(excludePath) {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(pattern + "\n")
	return err
}

// excludeResolveTimeout bounds the `git rev-parse` that resolves the exclude path.
// Wire runs on the launch hot-path where Inv 13 forbids HANGING the agent, so a
// wedged git (a stalled FUSE/network gitdir) aborts instead of blocking launch.
const excludeResolveTimeout = 3 * time.Second

// worktreeExcludePath resolves the info/exclude path git applies to worktreeDir.
// For a LINKED worktree this is the repo's SHARED (common) info/exclude (git has
// no per-worktree exclude), which still applies to the linked worktree. It returns
// an error when worktreeDir is not a git repo or git is unavailable, and is
// bounded so a wedged git can never hang launch.
func worktreeExcludePath(worktreeDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), excludeResolveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", worktreeDir, "rev-parse", "--git-path", "info/exclude")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git exclude path: %w", err)
	}
	rel := strings.TrimSpace(string(out))
	if rel == "" {
		return "", fmt.Errorf("empty git exclude path")
	}
	// `git -C <dir> rev-parse --git-path` yields a path relative to <dir> (or an
	// absolute path); join against worktreeDir so a relative result resolves.
	if !filepath.IsAbs(rel) {
		rel = filepath.Join(worktreeDir, rel)
	}
	return rel, nil
}

// excludeHasPattern reports whether the exclude file already contains pattern on
// its own line (so we never append a duplicate). A missing file is "no".
func excludeHasPattern(excludePath, pattern string) (bool, error) {
	b, err := os.ReadFile(excludePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == pattern {
			return true, nil
		}
	}
	return false, nil
}

// needsLeadingNewline reports whether the exclude file exists, is non-empty, and
// does NOT already end in a newline — in which case a separator must precede our
// appended pattern.
func needsLeadingNewline(excludePath string) bool {
	b, err := os.ReadFile(excludePath)
	if err != nil || len(b) == 0 {
		return false
	}
	return b[len(b)-1] != '\n'
}
