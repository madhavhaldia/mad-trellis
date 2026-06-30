package substrate

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Contain joins an untrusted relative path onto base and guarantees the result
// stays within base — the worktree-FS escape-resistance primitive (Inv 1 + the
// Inv-4 "sandbox-harder" tail). It rejects, per traversal vector:
//   - absolute paths (`/etc/passwd`),
//   - parent traversal (`../sibling`) that climbs out of base,
//   - symlinks whose resolved target leaves base.
//
// SCOPE — read this honestly. At the worktree GRAIN, Contain hardens the paths
// the substrate itself DERIVES; it is the guarantee that mad-substrate never HANDS
// an agent a path that escapes its boundary. It does NOT sandbox an agent that
// runs arbitrary shell to `cd /` and write elsewhere — a plain directory cannot
// structurally confine a process. Two seams carry the rest, by construction, not
// by oversight:
//   - Structural confinement of an UNCOOPERATIVE process is the CONTAINER/VM
//     grain (Inv 10-grainswap; Inv 4: "an agent that can't be enforced is
//     sandboxed harder"). The grain dial is that escalation MECHANISM, and it is
//     now REALIZED at the container tier (container.go): selecting the container
//     grain runs the agent inside a confined Linux container (cap-drop ALL,
//     read-only rootfs, --network none → no network namespace egress) with ONLY
//     its own worktree (plus its own per-agent writable state + an ephemeral /tmp)
//     reachable, so an agent that runs `cd /` and writes elsewhere is STRUCTURALLY
//     contained
//     — the C5/C13 chafe the worktree grain could only document is closed at that
//     tier. The git-commit-from-inside-container dev loop now CLOSES (the grain
//     provisions /work as a self-contained clone; commits land on the host via the
//     mount and `integrate --from`/harvest brings the branch back — see
//     container.go). The COOPERATIVE adapter reaching the daemon socket from inside
//     the container is now SHIPPED via a token-authed exec-stdio relay
//     (cmd/mad-substrate-relay + internal/launcher/coop.go) — Apple `container` has no
//     general host→container unix-socket forward, so the launcher tunnels the daemon
//     socket over a second `container exec`'s stdio. The relay ships EMBEDDED
//     (internal/coopembed) and auto-resolves, so the cooperative plane is ON BY
//     DEFAULT for the container grain; the HARD FLOOR still makes a container agent
//     SAFE even without it (it is fail-soft). The v1 container grain CONFINES, RUNS,
//     INTEGRATES, and now COOPERATES, which is the C5/C13 upgrade this tier owns.
//   - Severing the worktree's git-admin link to the host trunk is the
//     integrator's mediated remote (project 6, origin-bypass escape-resistance).
//
// So P2's escape-resistance is precisely: our own path handling is
// traversal-safe, and the env-spec we emit contains no path reaching a sibling
// boundary or the host trunk. The conformance harness (10a) re-asserts the full
// adversarial conjunct across all three grains/surfaces.
func Contain(base, untrusted string) (string, error) {
	if untrusted == "" {
		return "", fmt.Errorf("contain: empty path")
	}
	if filepath.IsAbs(untrusted) {
		return "", fmt.Errorf("contain: absolute path rejected: %q", untrusted)
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(baseAbs, untrusted)
	// Lexical containment (cheap; catches `..` before touching the filesystem).
	if !withinBase(baseAbs, joined) {
		return "", fmt.Errorf("contain: path escapes base: %q", untrusted)
	}
	// Symlink containment: resolve symlinks on the longest existing prefix of BOTH
	// the base and the joined path, then compare the resolved forms. Resolving the
	// base too is what keeps a base reached THROUGH a symlink (e.g. macOS
	// /var -> /private/var) from spuriously rejecting a genuinely-contained child.
	baseReal, err := resolveExisting(baseAbs)
	if err != nil {
		return "", err
	}
	resolved, err := resolveExisting(joined)
	if err != nil {
		return "", err
	}
	if !withinBase(baseReal, resolved) {
		return "", fmt.Errorf("contain: symlink escapes base: %q", untrusted)
	}
	return joined, nil
}

// withinBase reports whether p is baseAbs or strictly under baseAbs/ (lexical,
// after Clean). It is the single containment predicate used everywhere.
func withinBase(baseAbs, p string) bool {
	rel, err := filepath.Rel(baseAbs, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting resolves symlinks on the longest existing prefix of p, then
// re-appends the non-existent remainder (EvalSymlinks errors on a missing leaf,
// which is normal — the boundary dirs may not be created yet).
func resolveExisting(p string) (string, error) {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real, nil
	}
	parent := filepath.Dir(p)
	if parent == p { // reached the filesystem root
		return p, nil
	}
	realParent, err := resolveExisting(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(p)), nil
}

// disjoint reports whether two boundary roots are non-nested — neither contains
// the other. Sibling worktrees / state roots must never be reachable by walking
// from one into the other (Inv 1 cross-isolation).
func disjoint(a, b string) bool {
	aAbs, err1 := filepath.Abs(a)
	bAbs, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil || aAbs == bAbs {
		return false
	}
	return !withinBase(aAbs, bAbs) && !withinBase(bAbs, aAbs)
}
