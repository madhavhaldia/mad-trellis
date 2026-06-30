package integrator

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// trunkRepo conducts the git plumbing that advances the canonical trunk. It is
// the ONLY component with a path to the real trunk (the single-integrator
// guarantee, Inv 5-integrator / Inv 7): no agent worktree holds this path.
//
// The trunk advance is side-effect-free BY CONSTRUCTION — it never touches a
// working tree or index. A promote writes the merged tree + merge commit to the
// object store FIRST (no ref moves), then moves the branch ref in ONE atomic
// update-ref compare-and-swap. So git's ref store, not an external sweeper, is
// the source of truth for "promoted": trunk ref == merge_commit ⟺ promoted, and
// a death at any earlier point leaves the ref byte-identical (Inv 6/7 "by
// construction"). Because it is pure plumbing it works against a BARE trunk
// (the production mediated trunk) and a non-bare one alike.
type trunkRepo struct {
	dir    string // the trunk git directory (bare mediated repo in production)
	branch string // the trunk branch ref, e.g. "main"/"trunk"/"integration"
}

func newTrunkRepo(dir, branch string) *trunkRepo {
	return &trunkRepo{dir: dir, branch: branch}
}

func (t *trunkRepo) ref() string { return "refs/heads/" + t.branch }

// tip returns the current trunk commit (the ref's OID). ok=false if the trunk
// branch does not exist yet.
func (t *trunkRepo) tip() (oid string, ok bool, err error) {
	out, err := t.git("rev-parse", "--verify", "--quiet", t.ref())
	if err != nil {
		// rev-parse --quiet exits non-zero with empty output when the ref is absent.
		if strings.TrimSpace(out) == "" {
			return "", false, nil
		}
		return "", false, fmt.Errorf("trunk: rev-parse %s: %w: %s", t.ref(), err, out)
	}
	return strings.TrimSpace(out), true, nil
}

// resolve returns the commit OID a revision points at (e.g. a branch the agent
// pushed). It is read-only. --end-of-options ensures a ref that survived
// validRef but still looks option-like is treated strictly as a revision (defense
// in depth against git arg injection).
func (t *trunkRepo) resolve(rev string) (string, error) {
	out, err := t.git("rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("trunk: resolve %q: %w: %s", rev, err, out)
	}
	return strings.TrimSpace(out), nil
}

// mergeTree performs a real 3-way merge of branch into trunk WITHOUT touching
// any ref, index, or working tree, writing the merged tree to the object store.
// It is the deterministic validation primitive: a clean merge yields the tree
// OID (clean=true); a conflicting merge yields clean=false with the conflict
// summary; no probabilistic component is consulted (Inv 2(b)). trunkOID may be
// empty for an unborn trunk (first promote) — the branch tree is then adopted.
func (t *trunkRepo) mergeTree(trunkOID, branchOID string) (tree string, clean bool, summary string, err error) {
	if trunkOID == "" {
		// Unborn trunk: the merged tree is simply the branch's tree.
		out, gerr := t.git("rev-parse", "--verify", branchOID+"^{tree}")
		if gerr != nil {
			return "", false, "", fmt.Errorf("trunk: tree of %q: %w: %s", branchOID, gerr, out)
		}
		return strings.TrimSpace(out), true, "", nil
	}
	out, gerr := t.git("merge-tree", "--write-tree", trunkOID, branchOID)
	out = strings.TrimRight(out, "\n")
	if gerr == nil {
		// Exit 0: clean merge. First line is the merged tree OID.
		tree := firstLine(out)
		if tree == "" {
			return "", false, "", fmt.Errorf("trunk: merge-tree produced no tree")
		}
		return tree, true, "", nil
	}
	if ec, ok := exitCode(gerr); ok && ec == 1 {
		// Exit 1 is OVERLOADED: a real merge CONFLICT prints the merged tree OID on
		// the first line, whereas a git USAGE error (unrelated histories, bad object)
		// exits 1 with NO leading OID. Distinguish them so a genuine error is surfaced
		// as an error (retryable/internal) — never silently reported as a conflict
		// "validation failed" reject (a misreport, though always fail-CLOSED).
		if isHexOID(firstLine(out)) {
			return "", false, conflictSummary(out), nil
		}
		return "", false, "", fmt.Errorf("trunk: merge-tree (no merged tree): %s", strings.TrimSpace(out))
	}
	return "", false, "", fmt.Errorf("trunk: merge-tree: %w: %s", gerr, out)
}

// commitMerge writes a merge commit (first parent trunk, second parent branch)
// for the already-merged tree. It moves NO ref — the commit is reachable only
// once advanceTrunk runs. For an unborn trunk it writes a single-parent commit.
//
// The commit is a DETERMINISTIC function of (tree, parents, message, date): the
// author/committer identity and date are PINNED (date from the integration's
// stable CreatedAt), so two promotes of the SAME integration produce the SAME
// commit OID. That makes commitMerge idempotent under a concurrent same-id
// promote (no divergence between the durable merge_commit and the trunk ref) and
// removes any dependence on the bare repo's git user config.
func (t *trunkRepo) commitMerge(tree, trunkOID, branchOID, message string, date time.Time) (string, error) {
	args := []string{"commit-tree", tree, "-m", message}
	if trunkOID != "" {
		args = append(args, "-p", trunkOID)
	}
	args = append(args, "-p", branchOID)
	ds := date.UTC().Format("2006-01-02T15:04:05Z")
	cmd := exec.Command("git", append([]string{"-C", t.dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=mad-substrate", "GIT_AUTHOR_EMAIL=integrator@mad-substrate",
		"GIT_COMMITTER_NAME=mad-substrate", "GIT_COMMITTER_EMAIL=integrator@mad-substrate",
		"GIT_AUTHOR_DATE="+ds, "GIT_COMMITTER_DATE="+ds)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("trunk: commit-tree: %w: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// isAncestor reports whether commit is reachable from tip (an ancestor of, or
// equal to, tip). Used to reconcile a landed-then-superseded promote: once a
// merge commit is an ancestor of trunk it stays one (trunk only advances), so
// this is a STABLE predicate — unlike exact-tip equality, which only holds in
// the narrow window before the next promote moves trunk past it.
func (t *trunkRepo) isAncestor(commit, tip string) (bool, error) {
	if commit == "" || tip == "" {
		return false, nil
	}
	out, err := t.git("merge-base", "--is-ancestor", commit, tip)
	if err == nil {
		return true, nil
	}
	if ec, ok := exitCode(err); ok && ec == 1 {
		return false, nil // exit 1 = definitively NOT an ancestor
	}
	return false, fmt.Errorf("trunk: is-ancestor %s %s: %w: %s", short(commit), short(tip), err, out)
}

// isHexOID reports whether s is a bare git object id (40 hex for sha1, 64 for
// sha256) — used to tell a merge-tree conflict (OID first) from a usage error.
func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// advanceTrunk is the SINGLE atomic mutation that promotes: it moves the trunk
// ref from oldOID to newOID with a compare-and-swap (git update-ref's old-value
// precondition). It fails if the trunk moved since oldOID was read, so a stale
// promote can never clobber a concurrent advance. For an unborn trunk pass
// oldOID="" (create-only). This is the literal "by construction" commit point.
func (t *trunkRepo) advanceTrunk(newOID, oldOID string) error {
	args := []string{"update-ref", t.ref(), newOID}
	if oldOID == "" {
		// Create-only: the CAS old-value of all-zeros means "must not exist".
		args = append(args, "0000000000000000000000000000000000000000")
	} else {
		args = append(args, oldOID)
	}
	out, err := t.git(args...)
	if err != nil {
		return fmt.Errorf("trunk: update-ref CAS (%s %s->%s): %w: %s", t.ref(), short(oldOID), short(newOID), err, out)
	}
	return nil
}

func (t *trunkRepo) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", t.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// conflictSummary extracts a compact, deterministic conflict description from
// merge-tree's output (the portion after the blank line separating the tree OID
// from the conflict report), bounded so a pathological repo can't blow the log.
func conflictSummary(out string) string {
	if i := strings.Index(out, "\n\n"); i >= 0 {
		out = out[i+2:]
	}
	out = strings.TrimSpace(out)
	if out == "" {
		out = "merge conflict"
	}
	const max = 600
	if len(out) > max {
		out = out[:max] + "…"
	}
	return out
}

func exitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

func short(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	if oid == "" {
		return "∅"
	}
	return oid
}
