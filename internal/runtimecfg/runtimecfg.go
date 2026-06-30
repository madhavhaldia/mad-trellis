// Package runtimecfg is the SINGLE resolver for the mad-substrate per-user runtime
// directory and daemon socket, shared by EVERY Go surface (the CLI subcommands,
// the daemon, the shim dispatch). Centralizing it kills the prior drift where
// each command rolled its own `if socket=="" { socket = ~/.mad-substrate/... }` and
// only the daemon honored MAD_RUNTIME_DIR.
//
// Precedence, highest to lowest:
//   - socket:      explicit --socket flag  >  MAD_SOCKET  >  <runtime-dir>/daemon.sock
//   - runtime-dir: MAD_RUNTIME_DIR    >  MAD_HOME    >  ~/.mad-substrate
//
// The native cooperative subcommands (`mad-substrate mcp` / `hook`, via
// internal/coopclient) reuse this package directly, so a governed agent and the
// CLI always agree on the socket.
package runtimecfg

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// Source labels for doctor/diagnostic reporting. Exactly one is returned by the
// *Source helpers so an operator can see WHY a path was chosen.
const (
	SourceFlag       = "--socket flag"
	SourceEnvSocket  = "MAD_SOCKET"
	SourceRuntimeDir = "MAD_RUNTIME_DIR"
	SourceHome       = "MAD_HOME"
	SourceDefault    = "default (~/.mad-substrate)"
	SourcePerRepo    = "per-repo (auto)"
	socketBasename   = "daemon.sock"
)

// homeBaseDir is the global mad-substrate base (~/.mad-substrate), the root under which the
// bare default runtime AND per-repo runtimes live. Degrades to the temp dir when
// there is no home dir (rare), matching the old defaultRuntimeDir fallback.
func homeBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".mad-substrate")
}

// PerRepoRuntimeDir returns the per-repo runtime home for repoID: a stable,
// collision-resistant subdir of the global base (<home>/.mad-substrate/repos/<hash>,
// hash = sha256(repoID)[:8]). PURE — no git, no side effect, no MkdirAll — so it
// is unit-testable. The cmd layer resolves repoID (the repo's canonical, shared
// git COMMON dir — identical for the main worktree and every linked worktree of one
// repo) and, when no explicit runtime override is set, exports this as
// MAD_RUNTIME_DIR so EVERY surface (the CLI, the auto-started daemon, and a
// launched agent's adapter) agrees on ONE per-repo socket/ledger/trunk with no env
// juggling — and so a launched agent inherits the launcher's resolved runtime
// rather than mis-deriving it from its own boundary cwd. Returns "" for an empty
// repoID.
func PerRepoRuntimeDir(repoID string) string {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(repoID))
	return filepath.Join(homeBaseDir(), "repos", hex.EncodeToString(sum[:8]))
}

// resolveRuntimeDir is the pure (no side-effect) runtime-dir resolver. It never
// creates the directory; callers that need it on disk go through RuntimeDir.
func resolveRuntimeDir() (dir, source string) {
	if v := strings.TrimSpace(os.Getenv("MAD_RUNTIME_DIR")); v != "" {
		return v, SourceRuntimeDir
	}
	if v := strings.TrimSpace(os.Getenv("MAD_HOME")); v != "" {
		return v, SourceHome
	}
	return homeBaseDir(), SourceDefault
}

// RuntimeDir resolves the runtime dir AND ensures it exists (MkdirAll 0700),
// preserving the side-effect the daemon relies on (it derives ledger.db and the
// mediated trunk.git from this dir and assumes it is present). Returns the dir.
func RuntimeDir() string {
	dir, _ := resolveRuntimeDir()
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// RuntimeDirSource reports the resolved runtime dir and which input chose it,
// WITHOUT any side effect (no MkdirAll) — for doctor/diagnostics.
func RuntimeDirSource() (dir, source string) {
	return resolveRuntimeDir()
}

// SocketPath resolves the daemon socket. An explicit, non-empty flagOverride is
// the HIGHEST precedence (so the conformance harness's explicit --socket always
// wins); then MAD_SOCKET; else <runtime-dir>/daemon.sock.
//
// Side effects: the flag and env cases create NOTHING (the dir may intentionally
// live elsewhere). Only the default branch ensures the runtime dir exists (via
// RuntimeDir), preserving the daemon's expectation that the dir is present.
func SocketPath(flagOverride string) string {
	if v := strings.TrimSpace(flagOverride); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MAD_SOCKET")); v != "" {
		return v
	}
	return filepath.Join(RuntimeDir(), socketBasename)
}

// SocketSource reports the resolved socket and which input chose it, WITHOUT any
// side effect (no MkdirAll) — for doctor/diagnostics. The default-branch source
// is whichever runtime-dir input was used (so doctor shows the true origin).
func SocketSource(flagOverride string) (path, source string) {
	if v := strings.TrimSpace(flagOverride); v != "" {
		return v, SourceFlag
	}
	if v := strings.TrimSpace(os.Getenv("MAD_SOCKET")); v != "" {
		return v, SourceEnvSocket
	}
	dir, dsrc := resolveRuntimeDir()
	return filepath.Join(dir, socketBasename), dsrc
}

// Divergence reports whether BOTH MAD_RUNTIME_DIR and MAD_HOME are
// set (trimmed non-empty) AND differ — an ambiguous configuration where a process
// that resolves via MAD_HOME and one that resolves via MAD_RUNTIME_DIR
// could land on DIFFERENT sockets. doctor surfaces this so the mismatch is caught
// early.
func Divergence() (runtimeDir, home string, diverges bool) {
	rd := strings.TrimSpace(os.Getenv("MAD_RUNTIME_DIR"))
	hm := strings.TrimSpace(os.Getenv("MAD_HOME"))
	return rd, hm, rd != "" && hm != "" && rd != hm
}
