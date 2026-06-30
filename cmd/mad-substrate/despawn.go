package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-substrate/internal/substrate"
	"github.com/madhavhaldia/mad-substrate/internal/worktree"
)

// despawnCmd removes an abandoned `spawn` boundary's on-disk artifacts: the git
// worktree (worktree grain) or the self-contained clone (container grain), the
// nm/<slug> branch, and the per-agent state dir. `spawn` is the fire-and-forget
// bootstrap (chafe C6) — `mad-substrate launch` is the auto-cleaned path — so a
// boundary you spawned and decided NOT to integrate needs a manual cleanup.
//
// PURELY CLIENT-SIDE: it conducts git + filesystem only, adds NO daemon RPC (the
// registry is frozen), and touches no other session's boundary. The daemon's
// in-memory port/live-map reservation from a fire-and-forget spawn is freed on
// daemon restart (and never leaked by `launch`); despawn reclaims the durable
// on-disk artifacts. SAFETY: it refuses to discard uncommitted or unintegrated
// work unless --force.
func despawnCmd() *cobra.Command {
	var force bool
	var forceUnlock bool
	cmd := &cobra.Command{
		Use:   "despawn <branch|slug>",
		Short: "Remove an abandoned spawn boundary (worktree/clone + branch + state)",
		Long: "despawn removes the on-disk artifacts of a `spawn` boundary you are done with " +
			"(or decided not to integrate): the worktree or container clone, the nm/<slug> branch, and " +
			"the per-agent state dir. It is client-side git/fs only (no daemon RPC) and refuses to discard " +
			"uncommitted or unintegrated work unless --force. Accepts the spawn branch (nm/<slug>) or the " +
			"bare <slug>. Run it from the governed repo. (The transparent `mad-substrate launch` cleans up on " +
			"its own; despawn is for the fire-and-forget `spawn` bootstrap.)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := strings.TrimPrefix(strings.TrimSpace(args[0]), "nm/")
			if !isSafeSlug(slug) {
				return fmt.Errorf("invalid boundary id %q (expect a spawn branch nm/<slug> or its <slug>)", args[0])
			}
			repo, err := repoToplevel()
			if err != nil {
				return fmt.Errorf("cannot resolve the governed repo: %w", err)
			}
			path, err := worktree.Path(repo, slug)
			if err != nil || path == "" {
				return fmt.Errorf("cannot derive the boundary path for %q: %v", slug, err)
			}
			branch := "nm/" + slug
			out := cmd.OutOrStdout()

			_, statErr := os.Stat(path)
			onDisk := statErr == nil

			// The nm/<slug> BRANCH REF is the durable record of the agent's commits —
			// for the worktree grain it lives in the CANONICAL repo; for a container
			// clone it lives in the clone at `path`. The removal below deletes that
			// record, so the safety gate keys off the BRANCH TIP (NOT the worktree's
			// transient checked-out HEAD, which an agent may have moved with `git
			// checkout`) and runs whenever the branch OR the boundary exists — never
			// only "when the worktree dir is on disk".
			canonTip, canonHas := branchRefTip(repo, branch)

			// SAFETY (unless --force): never silently discard uncommitted or
			// UNINTEGRATED work. FAIL-CLOSED — an unprobeable boundary is treated as
			// unsafe rather than assumed clean.
			if !force {
				if onDisk {
					if dirty, derr := gitDirty(path); derr != nil || dirty {
						return fmt.Errorf("%s has uncommitted changes (or is unprobeable) — commit/integrate it, or re-run with --force to discard", path)
					}
				}
				// The branch tip, wherever it lives (canonical for the worktree grain;
				// the clone's own ref/HEAD for the container grain), must be integrated.
				workTip, haveWork := canonTip, canonHas
				if !haveWork && onDisk {
					if t, ok := branchRefTip(path, branch); ok {
						workTip, haveWork = t, true
					} else if t, ok := gitHead(path); ok {
						workTip, haveWork = t, true
					}
				}
				if haveWork && !commitIntegrated(repo, workTip) {
					return fmt.Errorf("branch %s has commits not integrated into the current trunk — `mad-substrate integrate %s` first, or re-run with --force to discard", branch, branch)
				}
			}

			var removed []string
			// (1) the worktree (linked → git worktree remove) or clone (plain dir → rm).
			if onDisk {
				if isLinkedWorktree(path) {
					forced, rmErr := removeLinkedWorktree(repo, path, forceUnlock)
					if rmErr != nil {
						// A FOREIGN lock surfaces a clear, actionable error and we return
						// BEFORE touching the branch/state — no partial state on refusal.
						return rmErr
					}
					if forced {
						removed = append(removed, "worktree "+path+" (force-unlocked)")
					} else {
						removed = append(removed, "worktree "+path)
					}
				} else {
					if rmErr := os.RemoveAll(path); rmErr != nil {
						return fmt.Errorf("remove clone %s: %w", path, rmErr)
					}
					removed = append(removed, "worktree "+path)
				}
			}
			// Prune a stale worktree admin entry BEFORE deleting the branch so a
			// lingering registration can't block `git branch -d` (it would say "used
			// by worktree").
			_, _ = runGit(repo, "worktree", "prune")
			// (2) the canonical branch ref, if present. Use `git branch -d` (lowercase)
			// on the non-forced path so GIT ITSELF refuses an unmerged branch — a second
			// backstop behind the gate above; --force uses -D (force).
			if canonHas {
				delFlag := "-d"
				if force {
					delFlag = "-D"
				}
				if _, derr := runGit(repo, "branch", delFlag, branch); derr == nil {
					removed = append(removed, "branch "+branch)
				} else if !force {
					return fmt.Errorf("git refused to delete branch %s (likely unmerged) — `mad-substrate integrate %s` first, or re-run with --force: %v", branch, branch, derr)
				}
			}
			// (3) the per-agent state dir.
			if sr := substrate.StateRoot(repo, slug); sr != "" {
				if _, srErr := os.Stat(sr); srErr == nil {
					if rmErr := os.RemoveAll(sr); rmErr == nil {
						removed = append(removed, "state "+sr)
					}
				}
			}

			if len(removed) == 0 {
				fmt.Fprintf(out, "nothing to remove for %s (already gone)\n", branch)
				return nil
			}
			fmt.Fprintf(out, "despawned %s:\n", branch)
			for _, r := range removed {
				fmt.Fprintf(out, "  removed %s\n", r)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even with uncommitted or unintegrated work (discards it)")
	cmd.Flags().BoolVar(&forceUnlock, "force-unlock", false, "if the worktree is LOCKED by a foreign tool (e.g. Supacode), unlock and remove it anyway")
	return cmd
}

// lockedWorktreeError is returned when a boundary's linked worktree is LOCKED by a
// FOREIGN tool (e.g. Supacode locks it with reason "owner:supacode") and despawn was
// invoked WITHOUT --force-unlock. The default refuses to override the lock: the work
// may already be integrated under the foreign tool's bookkeeping, so we report the
// lock + reason and stop rather than silently force or silently fail.
type lockedWorktreeError struct {
	path   string
	reason string
}

func (e *lockedWorktreeError) Error() string {
	reason := strings.TrimSpace(e.reason)
	if reason == "" {
		reason = "no reason recorded"
	}
	return fmt.Sprintf("worktree %s is LOCKED by a foreign tool (lock reason: %s) — its content may "+
		"already be integrated; removal was refused. Re-run with --force-unlock to unlock and remove it.",
		e.path, reason)
}

// removeLinkedWorktree removes the linked worktree at path, accounting for a FOREIGN
// lock (Inv 3: no mad-substrate lock outlives its holder — but a foreign tool's lock is
// not ours to break by default).
//
//   - If the worktree is NOT locked, it is removed exactly as before (worktree.Remove).
//   - If it IS locked and forceUnlock is false (DEFAULT), nothing is removed and a
//     lockedWorktreeError is returned so the caller refuses with a clear, actionable
//     message — no partial state.
//   - If it IS locked and forceUnlock is true, the lock is released with `git worktree
//     unlock` and the tree is removed with a DOUBLE `--force` (git requires `-f -f` to
//     drop a locked tree). forcedUnlock reports that this override happened.
//
// The path is always passed after a `--` separator so a (hypothetically) flag-like path
// can never be parsed as a git option.
func removeLinkedWorktree(repo, path string, forceUnlock bool) (forcedUnlock bool, err error) {
	locked, reason := worktreeLockReason(repo, path)
	if locked {
		if !forceUnlock {
			return false, &lockedWorktreeError{path: path, reason: reason}
		}
		// Best-effort explicit unlock first; then a double-force remove which git
		// requires for a still/again-locked tree. Both are scoped to THIS path.
		_, _ = runGit(repo, "worktree", "unlock", "--", path)
		if out, rerr := runGit(repo, "worktree", "remove", "--force", "--force", "--", path); rerr != nil {
			return false, fmt.Errorf("force-unlock+remove worktree %s: %v: %s", path, rerr, out)
		}
		return true, nil
	}
	return false, worktree.Remove(repo, path)
}

// worktreeLockReason reports whether the linked worktree at path is LOCKED (by git or a
// foreign tool) and, if so, the recorded reason. It parses `git worktree list
// --porcelain`: within the block for the target worktree, a bare `locked` line (or
// `locked <reason>`) marks it locked. Path comparison tolerates symlinked roots
// (/var → /private/var) so it matches git's canonicalized output.
func worktreeLockReason(repo, path string) (locked bool, reason string) {
	out, err := runGit(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false, ""
	}
	inTarget := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			inTarget = samePath(cur, path)
		case inTarget && (line == "locked" || strings.HasPrefix(line, "locked ")):
			return true, strings.TrimSpace(strings.TrimPrefix(line, "locked"))
		}
	}
	return false, ""
}

// samePath reports whether a and b name the same on-disk location, tolerating symlinked
// path roots (e.g. macOS /var → /private/var) that would defeat a raw string compare.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && ra == rb
}

// isSafeSlug reports whether s is a valid spawn slug ([A-Za-z0-9_-], non-empty) —
// safe as a path segment and a branch component, and guards every git/fs op below.
func isSafeSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// repoToplevel returns the MAIN worktree path (= the daemon's RepoRoot) so the
// boundary-path derivation matches, whether despawn is run from the main repo or a
// linked worktree. The main working tree is the first `git worktree list` entry.
func repoToplevel() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if out, gerr := runGit(wd, "worktree", "list", "--porcelain"); gerr == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "worktree ")), nil
			}
		}
	}
	if top, gerr := runGit(wd, "rev-parse", "--show-toplevel"); gerr == nil && strings.TrimSpace(top) != "" {
		return strings.TrimSpace(top), nil
	}
	return wd, nil
}

func gitDirty(dir string) (bool, error) {
	out, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func gitHead(dir string) (string, bool) {
	out, err := runGit(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// branchRefTip resolves refs/heads/<branch> in the git dir (the canonical repo, or
// a container clone) to its commit, reporting whether the branch exists. This is
// the commit the destructive path deletes — the safety gate must check THIS, not a
// worktree's (movable) checked-out HEAD.
func branchRefTip(gitDir, branch string) (string, bool) {
	out, err := runGit(gitDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return "", false
	}
	tip := strings.TrimSpace(out)
	if tip == "" {
		return "", false
	}
	return tip, true
}

// commitIntegrated reports whether commit is reachable from the canonical repo's
// current HEAD (already integrated). A commit the canonical repo does not even have
// (e.g. a container clone's un-integrated commits) yields a git error → NOT
// integrated, the safe default.
func commitIntegrated(repo, commit string) bool {
	_, err := runGit(repo, "merge-base", "--is-ancestor", commit, "HEAD")
	return err == nil
}

// isLinkedWorktree reports whether path is a git LINKED worktree (its .git is a
// FILE pointing into the canonical repo) vs a self-contained clone (its .git is a
// DIR). The worktree grain produces the former; the container grain a clone.
func isLinkedWorktree(path string) bool {
	fi, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return !fi.IsDir()
}
