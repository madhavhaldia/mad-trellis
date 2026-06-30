package integrator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// mediated.go implements the agent-facing half of Inv 7: each worktree's "origin"
// is redirected to a mad-substrate-mediated HOLDING repo (a bare repo mad-substrate
// owns), and the integrator is the sole party that bridges the holding repo to
// real trunk. An agent's natural `git push` lands in the holding repo, never at
// the canonical trunk / real origin.
//
// ORIGIN-BYPASS escape-resistance is delivered as an AUDITABLE property (the
// analog of the substrate's "never HAND an agent an escape", chafe C5): the
// integrator never configures a remote/insteadOf that reaches canonical, and
// AuditWorktreeRemotes detects one if present (with a positive control). HONEST
// SCOPE (chafe sibling of C5): at the v1 LINKED-worktree grain the worktree
// shares the canonical object store and its `.git` file names the canonical
// gitdir, so a determined UNCOOPERATIVE agent that derives that path can still
// reach canonical objects. STRUCTURAL severing (a standalone clone / container /
// VM grain) is the grain dial; 10a re-asserts the full adversarial conjunct.
// What v1 guarantees structurally: trunk advances ONLY through the integrator's
// lease-gated atomic promote — no cooperative push path advances trunk.

// EnsureMediatedRepo creates (idempotently) the bare holding repo at dir — the
// agent-facing "origin" — with trunkBranch as its protected trunk ref. It
// installs a receive `update` hook that lets agents push ONLY their own
// refs/heads/nm/* branches and REJECTS any push to the protected trunk ref
// (default-deny for everything else). The integrator advances trunk via a LOCAL
// update-ref, which does not fire receive hooks, so this hook structurally
// enforces "no agent advances trunk by pushing" (Inv 7) without constraining the
// integrator. It returns the absolute path. The hook is (re)written every call
// so an upgrade repairs a stale/missing hook.
func EnsureMediatedRepo(dir, trunkBranch string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !isGitRepo(abs) {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
		if out, err := gitAt("", "init", "--bare", "-b", trunkBranch, abs); err != nil {
			return "", fmt.Errorf("mediated: init bare %q: %w: %s", abs, err, out)
		}
	}
	if err := installTrunkProtectHook(abs, trunkBranch); err != nil {
		return "", err
	}
	return abs, nil
}

// installTrunkProtectHook writes the receive `update` hook that protects the
// trunk ref from agent pushes. It is invoked once per pushed ref with
// (refname, old, new); a non-zero exit rejects that ref update.
func installTrunkProtectHook(bareRepo, trunkBranch string) error {
	hooksDir := filepath.Join(bareRepo, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	script := `#!/bin/sh
# mad-substrate mediated holding repo (project 6, Inv 7): agents may push ONLY their
# own refs/heads/nm/* branches; the protected trunk ref is advanced solely by the
# integrator via a LOCAL update-ref (which does not run receive hooks). Any other
# ref push is denied (default-deny).
ref="$1"
case "$ref" in
  refs/heads/` + trunkBranch + `)
    echo "mad-substrate: $ref is integrator-only — no agent push (Inv 7)" >&2
    exit 1 ;;
  refs/heads/nm/*)
    exit 0 ;;
  refs/mad-substrate/*)
    exit 0 ;;
  *)
    echo "mad-substrate: pushing $ref is not permitted (agents push refs/heads/nm/*)" >&2
    exit 1 ;;
esac
`
	return os.WriteFile(filepath.Join(hooksDir, "update"), []byte(script), 0o755)
}

// RedirectRemote points a workspace's `origin` at the mediated holding repo,
// REPLACING any inherited real-origin URL (so it cannot leak through) with a
// single-valued local origin, then verifies the redirect took.
//
// HONEST SCOPE (chafe, sibling of C5/the origin-bypass scope note above): a
// REPLACE in local config is clean for a STANDALONE workspace (the clone /
// container / VM grain). A v1 LINKED worktree SHARES the canonical repo's config
// with the main checkout, so a per-worktree origin cannot be set without
// disturbing the canonical's own remote — full per-worktree redirect is the
// clone/container grain (the grain dial), not the linked-worktree grain. The
// structural Inv-7 guarantee at v1 is the integrator-only trunk advance + the
// holding repo's trunk-protect hook, not this cooperative redirect.
func RedirectRemote(worktree, mediatedDir string) error {
	med, err := filepath.Abs(mediatedDir)
	if err != nil {
		return err
	}
	// Replace any inherited origin url (single-valued) with the mediated repo; if
	// origin is absent, add it. set-url overwrites rather than appending a multivar
	// (which would leave the inherited url winning for `git push`).
	if _, err := gitAt(worktree, "remote", "set-url", "origin", med); err != nil {
		if out, addErr := gitAt(worktree, "remote", "add", "origin", med); addErr != nil {
			return fmt.Errorf("mediated: set origin: %w: %s", addErr, out)
		}
	}
	// Also pin the PUSH url to the mediated repo. `git push` prefers
	// remote.origin.pushurl over .url, so an inherited pushurl pointing at the real
	// origin would silently survive a .url-only redirect — set it explicitly.
	if out, err := gitAt(worktree, "remote", "set-url", "--push", "origin", med); err != nil {
		return fmt.Errorf("mediated: set origin pushurl: %w: %s", err, out)
	}
	for _, kind := range [][]string{{"remote", "get-url", "origin"}, {"remote", "get-url", "--push", "origin"}} {
		got, err := gitAt(worktree, kind...)
		if err != nil {
			return fmt.Errorf("mediated: verify origin: %w: %s", err, got)
		}
		if strings.TrimSpace(got) != med {
			return fmt.Errorf("mediated: origin redirect failed: got %q want %q", strings.TrimSpace(got), med)
		}
	}
	return nil
}

// RemoteAudit is the result of an origin-bypass escape-resistance scan.
type RemoteAudit struct {
	Clean      bool
	URLs       map[string]string // config key -> url
	Violations []RemoteViolation
}

// RemoteViolation is one configured git target that reaches a forbidden path.
type RemoteViolation struct {
	ConfigKey string
	URL       string
	Matched   string // the forbidden target it resolved to
}

// AuditWorktreeRemotes scans EVERY configured git target of a worktree — all
// `remote.<name>.url` and `url.<base>.insteadOf` rewrites — and reports any that
// reaches a forbidden path (the canonical trunk / real origin). It is the
// origin-bypass escape-resistance check; its positive control adds a remote
// pointing at a forbidden path and asserts Clean goes false. FS targets are
// compared by resolved absolute path; non-FS targets (ssh/https) by literal
// containment of a forbidden token.
func AuditWorktreeRemotes(worktree string, forbidden []string) (RemoteAudit, error) {
	// Scan every configured git TARGET: fetch urls (remote.*.url), PUSH urls
	// (remote.*.pushurl — git push prefers these, so an inherited one is a real
	// escape the url-only scan would miss), and url.*.insteadof / pushinsteadof
	// rewrites (the `insteadof$` suffix matches both).
	out, err := gitAt(worktree, "config", "--get-regexp", `(^remote\..*\.(url|pushurl)$|insteadof$)`)
	audit := RemoteAudit{Clean: true, URLs: map[string]string{}}
	if err != nil {
		// Exit 1 with no output = no matching keys (no remotes configured) → clean.
		if strings.TrimSpace(out) == "" {
			return audit, nil
		}
		return RemoteAudit{}, fmt.Errorf("mediated: read git config: %w: %s", err, out)
	}
	norm := normalizedForbidden(forbidden)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		audit.URLs[key] = val
		if m, hit := reachesForbidden(val, norm); hit {
			audit.Clean = false
			audit.Violations = append(audit.Violations, RemoteViolation{ConfigKey: key, URL: val, Matched: m})
		}
	}
	return audit, nil
}

// normalizedForbidden resolves FS-path forbidden targets to absolute, symlink-
// evaluated form so a remote written with a different-but-equivalent spelling is
// still caught; non-path tokens are kept verbatim for containment matching.
func normalizedForbidden(forbidden []string) map[string]string {
	out := map[string]string{}
	for _, f := range forbidden {
		if f == "" {
			continue
		}
		out[f] = f // raw token (for url containment, e.g. github.com/org/repo)
		if abs := resolvePath(f); abs != "" {
			out[abs] = f
		}
	}
	return out
}

func reachesForbidden(url string, forbidden map[string]string) (string, bool) {
	cand := url
	cand = strings.TrimPrefix(cand, "file://")
	if abs := resolvePath(cand); abs != "" {
		if orig, ok := forbidden[abs]; ok {
			return orig, true
		}
	}
	for tok, orig := range forbidden {
		if strings.Contains(url, tok) {
			return orig, true
		}
	}
	return "", false
}

func resolvePath(p string) string {
	if p == "" || strings.Contains(p, "://") {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if ev, err := filepath.EvalSymlinks(abs); err == nil {
		return ev
	}
	return abs
}

func isGitRepo(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return true // bare repo
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	return false
}

func gitAt(dir string, args ...string) (string, error) {
	var full []string
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
