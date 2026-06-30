package substrate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/madhavhaldia/mad-substrate/internal/repoid"
)

// StateRoot returns the per-agent local-state root for (repoAbs, slug) WITHOUT
// creating it — the SAME deterministic derivation provisionState uses. Exported so
// a client-side teardown (`mad-substrate despawn`) can reclaim an abandoned spawn's
// state dir without the daemon. Returns "" for an empty or unsafe slug (one that
// could traverse outside the per-repo state base).
func StateRoot(repoAbs, slug string) string {
	if slug == "" || strings.ContainsAny(slug, `/\`) || strings.Contains(slug, "..") {
		return ""
	}
	return filepath.Join(stateBase(repoAbs), slug)
}

// stateRoles are the per-agent local-state directories the substrate isolates.
// Inv 1 covers "local state", not only the worktree: scratch (TMPDIR), cache,
// and state must each be private per agent so two agents never read/write a
// shared scratch/cache path.
var stateRoles = []string{"scratch", "cache", "state"}

// stateBase returns the per-repo local-state root OUTSIDE the repo tree, keyed
// by a hash of the repo's CANONICAL git identity (shared by all checkouts of the
// repo — see internal/repoid; chafe C2: per-agent state must never land in the
// governed repo and dirty its `git status`). Override with MAD_STATE_DIR.
func stateBase(repoAbs string) string {
	root := os.Getenv("MAD_STATE_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		root = filepath.Join(home, ".mad-substrate", "state")
	}
	h := sha256.Sum256([]byte(repoid.Canonical(repoAbs)))
	return filepath.Join(root, hex.EncodeToString(h[:8]))
}

// provisionState creates this agent's disjoint scratch/cache/state dirs under a
// per-session root and returns (role->path, agentRoot). Every role path is
// derived through Contain, so no role name can traverse out of the agent's own
// state root (escape-resistance for the substrate's own path handling).
func provisionState(repoAbs, slug string) (dirs map[string]string, agentRoot string, err error) {
	agentRoot = filepath.Join(stateBase(repoAbs), slug)
	dirs = make(map[string]string, len(stateRoles))
	for _, role := range stateRoles {
		p, cerr := Contain(agentRoot, role)
		if cerr != nil {
			// Return agentRoot even on failure so Provision's rollback can clean any
			// partial dirs (otherwise a mid-loop failure leaks scratch/cache).
			return nil, agentRoot, cerr
		}
		if mkErr := os.MkdirAll(p, 0o700); mkErr != nil {
			return nil, agentRoot, mkErr
		}
		dirs[role] = p
	}
	return dirs, agentRoot, nil
}

// removeState deletes an agent's entire local-state root. Teardown MECHANISM
// only (the launcher/liveness decide WHEN). Idempotent.
func removeState(agentRoot string) error {
	if agentRoot == "" {
		return nil
	}
	return os.RemoveAll(agentRoot)
}
