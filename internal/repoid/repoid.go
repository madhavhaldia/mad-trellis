// Package repoid resolves a repo's CANONICAL identity — the absolute,
// symlink-resolved git common directory (`git rev-parse --git-common-dir`),
// which is IDENTICAL for every worktree/checkout of one repo. Hashing this
// (instead of a per-checkout absolute path) makes per-repo storage stable
// across checkouts and daemon restarts. cgo-free; stdlib + os/exec only.
package repoid

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// Canonical returns the canonical identity of the repo at repoPath: the
// absolute, symlink-resolved git common dir. On ANY failure (not a git repo,
// git missing) it falls back to the absolute repoPath — DETERMINISTIC and never
// empty, so non-repo callers and tests behave exactly as the old path-keying did.
func Canonical(repoPath string) string {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	out, gerr := exec.Command("git", "-C", abs, "rev-parse", "--git-common-dir").Output()
	if gerr != nil {
		return abs // fallback: previous per-path behavior
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return abs
	}
	// With `-C abs`, a relative result (commonly ".git") is relative to abs —
	// anchor it to abs (NOT the process cwd).
	if !filepath.IsAbs(p) {
		p = filepath.Join(abs, p)
	}
	if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
		return resolved
	}
	return p
}
