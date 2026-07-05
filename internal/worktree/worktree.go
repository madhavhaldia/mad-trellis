// Package worktree creates and removes per-agent git worktrees — the v1 grain's
// filesystem mechanism for the isolation substrate (project 4). It CONDUCTS
// `git worktree` (never reimplements git — GROUNDING "conduct, don't reimplem-
// ent", and go-git/v5 does not support linked worktrees, see chafe). The
// worktree is named by a caller-supplied SAFE identifier — the daemon's
// unspoofable session slug (Inv 4) — never a self-minted one, so the boundary is
// bound to the governed session (P2 top risk: key worktree naming off the daemon
// session). Per-agent runtime env, ports, local-state, and escape-resistance are
// composed OVER this by internal/substrate.
package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/madhavhaldia/mad-trellis/internal/repoid"
)

// Worktree is a created per-agent worktree.
type Worktree struct {
	Name   string // the caller-supplied safe identifier (the session slug)
	Branch string
	Path   string
}

// Create adds a worktree off the repo's current HEAD on a fresh branch
// nm/<name>, located OUTSIDE the repo tree so spawning never pollutes the repo's
// `git status` and never touches a project file (Inv 11). `name` must already be
// a filesystem- and git-ref-safe identifier (the substrate slugifies the daemon
// session before calling) — Create validates and rejects an unsafe name rather
// than silently rewriting it.
func Create(repo, name string) (*Worktree, error) {
	if !safeName(name) {
		return nil, fmt.Errorf("worktree: unsafe name %q (want [A-Za-z0-9_-]+)", name)
	}
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	branch := "nm/" + name
	base := worktreeBase(repoAbs)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(base, name)
	if out, err := git(repo, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		return nil, fmt.Errorf("git worktree add: %w: %s", err, out)
	}
	return &Worktree{Name: name, Branch: branch, Path: path}, nil
}

// CreateClone makes a STANDALONE clone of the repo at the SAME deterministic
// per-agent path Create uses, on a fresh branch nm/<name> — the CONTAINER grain's
// filesystem mechanism. The worktree grain's Create makes a LINKED worktree whose
// `.git` is a FILE pointing back into the canonical repo's `.git` (OUTSIDE a
// container bind-mount), so git inside the container cannot resolve it. A clone is
// SELF-CONTAINED: `--no-hardlinks` COPIES the objects into <path>/.git (the
// default local clone hardlinks them into the canonical .git, which would dangle
// inside the mount), so an agent whose only mount is this dir can commit against a
// real, complete repository. `--single-branch` bounds the copy to the current
// branch's history. The clone's `origin` is REMOVED (it names the canonical repo
// path, which is NOT mounted into the container anyway) so the agent works purely
// locally — integration is host-mediated (`integrate --from` / the container
// grain's harvest-on-teardown), never the agent reaching the canonical repo. A
// default identity is set so in-container commits succeed even on an image with no
// git config. CONFINEMENT is UNCHANGED versus the linked worktree: nothing extra
// is mounted; the clone is the agent's own /work, already its only writable repo.
func CreateClone(repo, name string) (*Worktree, error) {
	if !safeName(name) {
		return nil, fmt.Errorf("worktree: unsafe name %q (want [A-Za-z0-9_-]+)", name)
	}
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	branch := "nm/" + name
	base := worktreeBase(repoAbs)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(base, name)
	// A stale dir at the deterministic path (a crashed prior provision) makes
	// `git clone` refuse a non-empty target — clear it first so re-provision after a
	// crash is idempotent (mirrors the container grain's stale-name container prune).
	if _, statErr := os.Stat(path); statErr == nil {
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return nil, fmt.Errorf("worktree: clear stale clone %q: %w", path, rmErr)
		}
	}
	if out, err := git(repoAbs, "clone", "--no-hardlinks", "--single-branch", "--quiet", repoAbs, path); err != nil {
		return nil, fmt.Errorf("git clone: %w: %s", err, out)
	}
	// A fresh branch off the cloned HEAD (the agent's workspace), exactly as Create's
	// `worktree add -b nm/<name> ... HEAD` does.
	if out, err := git(path, "checkout", "-q", "-b", branch); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("git checkout -b %s: %w: %s", branch, err, out)
	}
	// Drop the origin so the agent has NO remote pointing at the canonical repo
	// (purely local commits; integration is host-mediated). Tolerate a clone that
	// somehow has no origin.
	if out, err := git(path, "remote", "remove", "origin"); err != nil &&
		!strings.Contains(strings.ToLower(out), "no such remote") {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("git remote remove origin: %w: %s", err, out)
	}
	// A default identity so commits succeed without per-image git config; the agent
	// is free to override it.
	_, _ = git(path, "config", "user.email", "mad-trellis-agent@local")
	_, _ = git(path, "config", "user.name", "mad-trellis agent")
	return &Worktree{Name: name, Branch: branch, Path: path}, nil
}

// Path returns the DETERMINISTIC worktree path Create would (or did) use for
// repo + name, WITHOUT creating anything. It is the reconstruction primitive a
// grain needs to reclaim an ORPHANED worktree after a daemon restart (the live
// map is gone, but the path is derivable from the session slug alone). Returns
// "" for an unsafe name (Create would have rejected it, so no such worktree exists).
func Path(repo, name string) (string, error) {
	if !safeName(name) {
		return "", fmt.Errorf("worktree: unsafe name %q", name)
	}
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(worktreeBase(repoAbs), name), nil
}

// worktreeBase returns a per-repo worktree root OUTSIDE the repo tree, keyed by
// a hash of the repo's CANONICAL git identity (shared by all checkouts of the
// repo — see internal/repoid). Override with MAD_WORKTREE_DIR.
func worktreeBase(repoAbs string) string {
	root := os.Getenv("MAD_WORKTREE_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		root = filepath.Join(home, ".mad-trellis", "worktrees")
	}
	h := sha256.Sum256([]byte(repoid.Canonical(repoAbs)))
	return filepath.Join(root, hex.EncodeToString(h[:8]))
}

// Remove deletes a worktree and prunes its admin entry. The branch is left
// intact (its commits may not yet be integrated). It is the teardown MECHANISM;
// WHEN to call it (clean exit / crash) is the launcher's / liveness's job.
func Remove(repo, path string) error {
	if out, err := git(repo, "worktree", "remove", "--force", path); err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, out)
	}
	return nil
}

// safeName reports whether s is a non-empty identifier of only [A-Za-z0-9_-] —
// safe as both a path segment and a git branch component.
func safeName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
