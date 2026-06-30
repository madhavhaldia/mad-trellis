package launcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SupportedAgents are the agent CLIs the transparent shim governs. Claude Code
// is instance #1 (docs/0004 open issue #4), but this is DATA, not coupling:
// adding a name installs another shim, and there is zero agent-specific logic
// anywhere in the launcher (Inv 10 — couple to no agent). The shim re-invokes
// the mad-substrate binary under the agent's name; main() detects that and routes
// the call through the governed launcher.
var SupportedAgents = []string{"claude", "codex"}

// ErrShimTampered is a fail-closed condition: the real agent binary cannot be
// resolved without resolving back to the mad-substrate shim (which would loop), so
// the launcher refuses rather than exec something ungoverned or recursive.
var ErrShimTampered = errors.New("shim: cannot resolve the real agent binary (only the shim is reachable)")

// ErrAgentNotFound means no real agent binary exists on PATH outside the shim
// dir. Fail-closed: the launcher reports it rather than falling through.
var ErrAgentNotFound = errors.New("shim: no real agent binary found on PATH")

// IsSupportedAgent reports whether name is a governed agent CLI.
func IsSupportedAgent(name string) bool {
	for _, a := range SupportedAgents {
		if a == name {
			return true
		}
	}
	return false
}

// AgentFromArgv0 returns the governed agent name when mad-substrate was invoked
// THROUGH a shim — i.e. argv[0]'s basename is a supported agent rather than
// "mad-substrate". Returns "" for a normal `mad-substrate ...` invocation.
func AgentFromArgv0(argv0 string) string {
	base := filepath.Base(argv0)
	// Strip a possible platform suffix; v1 is darwin so this is a no-op, kept for
	// portability of the basename match.
	base = strings.TrimSuffix(base, ".exe")
	if base == "mad-substrate" {
		return ""
	}
	if IsSupportedAgent(base) {
		return base
	}
	return ""
}

// DefaultShimDir is where InstallShims places the shim executables by default.
func DefaultShimDir(runtimeDir string) string { return filepath.Join(runtimeDir, "shims") }

// InstallShims creates, in dir, one shim per agent that re-invokes madSubstrateBin
// under the agent's name (a symlink to the mad-substrate binary). It writes ONLY
// into dir — it never edits the user's shell rc (PATH interposition is opt-in:
// the caller prints the line for the user to eval). Idempotent: an existing
// shim is replaced. Returns the installed shim paths.
func InstallShims(madSubstrateBin, dir string, agents []string) ([]string, error) {
	binAbs, err := filepath.Abs(madSubstrateBin)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var installed []string
	for _, a := range agents {
		p := filepath.Join(dir, a)
		// Replace any existing entry so install is idempotent and a stale shim
		// pointing at an old binary can't survive.
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("shim install %s: %w", a, err)
		}
		if err := os.Symlink(binAbs, p); err != nil {
			return nil, fmt.Errorf("shim install %s: %w", a, err)
		}
		installed = append(installed, p)
	}
	return installed, nil
}

// ResolveReal finds the REAL agent binary on pathEnv, EXCLUDING the shim dir and
// the mad-substrate binary itself, so the launcher never re-execs the shim (which
// would loop forever). It walks pathEnv in order and returns the first entry
// whose <entry>/<agent> is an executable regular file that does NOT resolve back
// to the mad-substrate binary. If the only reachable match IS a shim, it returns
// ErrShimTampered; if none is found at all, ErrAgentNotFound — both fail-closed,
// never a fall-through to an ungoverned or self-recursive exec.
//
// selfBin is the running mad-substrate binary path (os.Executable()), resolved
// through symlinks for the comparison so a shim symlink → mad-substrate is detected.
//
// FAIL-CLOSED on an unknown self (the cardinal rule): if selfBin cannot be
// resolved to a real path, we cannot reliably tell a shim apart from a real
// agent, so we REFUSE rather than risk returning a shim and re-exec'ing mad-substrate
// forever. The caller (resolveAgentBinary) must not pass an empty selfBin — it
// turns an os.Executable() error into a BLOCK — but this guard makes the failure
// mode safe regardless of caller.
func ResolveReal(agent, shimDir, selfBin, pathEnv string) (string, error) {
	selfReal := resolveSymlinks(selfBin)
	if selfReal == "" {
		return "", fmt.Errorf("%w: cannot identify the mad-substrate binary to exclude the shim", ErrShimTampered)
	}
	shimReal := resolveSymlinks(shimDir)
	sawShim := false
	for _, entry := range strings.Split(pathEnv, string(os.PathListSeparator)) {
		if entry == "" {
			continue
		}
		// Exclude the shim dir by RESOLVED real path, so a PATH entry that reaches
		// the shim dir via a symlink/relative/aliased path (symlinked $HOME,
		// /tmp→/private/tmp, a second link to the same dir) is still excluded —
		// the literal-string compare missed those and let a shim through.
		if resolveSymlinks(entry) == shimReal {
			continue
		}
		cand := filepath.Join(entry, agent)
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue // missing, a dir, or not executable
		}
		// A candidate that resolves to the mad-substrate binary is a shim (an installed
		// symlink, or a stray shim in a dir != shimDir). Skip it and remember we saw
		// one, so an all-shim PATH fails closed instead of looping.
		if resolveSymlinks(cand) == selfReal {
			sawShim = true
			continue
		}
		return cand, nil
	}
	if sawShim {
		return "", ErrShimTampered
	}
	return "", ErrAgentNotFound
}

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return filepath.Clean(a)
	}
	return filepath.Clean(p)
}

func resolveSymlinks(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return absClean(p)
}
