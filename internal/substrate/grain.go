package substrate

import (
	"os"
	"strings"
	"sync"

	"github.com/madhavhaldia/mad-substrate/internal/worktree"
)

// Grain is the isolation DIAL (Inv 10-grainswap): worktree (v1) → container → VM,
// all behind ONE interface so the Substrate and every caller are unchanged when
// the backend swaps. v1 builds the DIAL, not the escalation POLICY (when to move
// up a grain is the open grain-escalation question, carried under "none in v1").
//
// A grain provisions only the per-agent FILESYSTEM boundary; the Substrate
// composes ports, local-state, env, and classifier-gated resource routing OVER
// it grain-agnostically. (At a container/VM grain, ports would be namespaced and
// the Substrate's host-port allocation becomes a no-op — a documented seam, not a
// behavior the caller sees.)
type Grain interface {
	// Name identifies the grain ("worktree", "container", "vm").
	Name() string
	// Provision creates the filesystem boundary for the given session SLUG (a
	// pre-sanitized, unique-per-session identifier the Substrate derives from the
	// daemon's unspoofable session id).
	Provision(slug string) (Boundary, error)
	// Teardown removes the boundary. MECHANISM only — WHEN is the launcher's
	// (clean exit) or liveness's (crash) call. Must be idempotent-friendly.
	Teardown(b Boundary) error
	// ReclaimOrphan tears down a boundary the grain can RECONSTRUCT from the
	// session SLUG alone — used when no live Boundary survives (a daemon restart
	// reattaching from the durable lease ledger, where the in-memory live map is
	// fresh/empty). It must be best-effort and idempotent: a grain reconstructs
	// the per-session resources by their DETERMINISTIC names/paths and removes any
	// that exist, treating "already gone" as success. This is what makes liveness's
	// restart-reattachment actually reclaim an orphaned container/worktree instead
	// of leaking it. NORMAL teardown (with a live Boundary) goes through Teardown.
	ReclaimOrphan(slug string) error
}

// Boundary is the filesystem boundary a grain produced.
type Boundary struct {
	Cwd    string // the agent's working directory (the in-container path at the container grain)
	Branch string // the worktree branch (empty for non-git grains)
	// HostWorktree is the host-side path of the agent's git worktree. At the
	// worktree grain it equals Cwd; at the container grain Cwd is the in-container
	// mount point (/work) while HostWorktree is the host dir bind-mounted there —
	// Teardown removes THIS, and the disjoint-from-repo check runs against it.
	HostWorktree string
	// ContainerID is the id of the confined container the agent execs into (empty
	// for non-container grains). Carried so the launcher can `container exec` into
	// the boundary and so Teardown can `container rm -f` it.
	ContainerID string
}

// worktreeGrain is the v1 grain: a git worktree per agent, outside the repo
// tree, conducted via internal/worktree.
//
// CONCURRENCY (chafe — caught by the concurrent-provision invariant test): `git
// worktree add`/`remove` are NOT safe to run in parallel on one repo — they
// mutate shared `.git/worktrees/` admin state and concurrent runs corrupt each
// other (`failed to read .git/worktrees/<x>/commondir`). Unlike the lease ledger
// (SQLite single-writer) the grain conducts an external tool with its own
// concurrency model, so the grain SERIALIZES its git operations. The slow git op
// is held under THIS mutex only — not the substrate's live-map lock — so port
// allocation and state setup for other sessions still proceed concurrently.
type worktreeGrain struct {
	repo string
	mu   sync.Mutex
}

func (g *worktreeGrain) Name() string { return "worktree" }

func (g *worktreeGrain) Provision(slug string) (Boundary, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	wt, err := worktree.Create(g.repo, slug)
	if err != nil {
		return Boundary{}, err
	}
	// At the worktree grain the agent works directly in the host worktree, so Cwd
	// and HostWorktree coincide.
	return Boundary{Cwd: wt.Path, Branch: wt.Branch, HostWorktree: wt.Path}, nil
}

func (g *worktreeGrain) Teardown(b Boundary) error {
	path := b.HostWorktree
	if path == "" {
		path = b.Cwd // back-compat: a boundary built before HostWorktree existed
	}
	if path == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return worktree.Remove(g.repo, path)
}

// ReclaimOrphan reconstructs the worktree path from the slug (worktree.Path is
// the deterministic derivation Create uses) and removes it if it still exists.
// Best-effort + idempotent: an already-gone or never-created worktree is benign,
// so a restart-reattachment that reconstructs a dead holder reclaims its stale
// worktree dir without leaking it. Holds the grain mutex (git worktree ops are
// not parallel-safe), mirroring Teardown.
func (g *worktreeGrain) ReclaimOrphan(slug string) error {
	path, err := worktree.Path(g.repo, slug)
	if err != nil || path == "" {
		return err
	}
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		// Nothing on disk — already reclaimed (or never created). Prune any stale
		// admin entry left behind, but treat its absence as success.
		g.mu.Lock()
		defer g.mu.Unlock()
		_ = worktree.Remove(g.repo, path)
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if rmErr := worktree.Remove(g.repo, path); rmErr != nil && !worktreeNotFound(rmErr) {
		return rmErr
	}
	return nil
}

// worktreeNotFound reports whether a `git worktree remove` failure is the benign
// already-gone case, so ReclaimOrphan stays idempotent.
func worktreeNotFound(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "is not a working tree") ||
		strings.Contains(m, "not a working tree") ||
		strings.Contains(m, "no such") ||
		strings.Contains(m, "not found")
}
