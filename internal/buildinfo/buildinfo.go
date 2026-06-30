// Package buildinfo is the pure, cgo-free home for mad-substrate's version-pin
// manifest and the version/doctor reporting logic. It exists so the substantive
// checks — manifest loading, dotted-version comparison, git floor enforcement,
// embedded module-version extraction, and the `version` render shape — are unit
// tested in isolation, while the CLI surfaces (cmd/mad-substrate/version + doctor)
// stay thin (dial + print + exit).
//
// SINGLE SOURCE OF TRUTH: versions.json pins everything NOT already expressed in
// go.mod (the conducted git floor, the supported platforms). The canonical copy
// lives at the repo ROOT and is the only one humans edit; the copy embedded here
// is auto-derived from it by `go generate ./internal/buildinfo/...` (go:embed
// cannot reach a parent directory, so we cannot embed the root copy directly).
// Flow: edit the ROOT versions.json, then `go generate ./internal/buildinfo/...`;
// the drift test fails CI if the two diverge. internal/buildinfo/drift_test.go
// asserts the embedded copy, the root copy, and go.mod never drift apart
// (chafe C18).
package buildinfo

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// Keeping the two copies in sync: edit the ROOT versions.json (the canonical
// single source of truth), then run `go generate ./internal/buildinfo/...`; the
// generator (gen.go) copies the repo-root versions.json over this package-local
// copy byte-for-byte. drift_test.go's TestEmbeddedManifestMatchesRoot fails CI if
// the two ever diverge (i.e. if someone edits the root copy but forgets to run
// generate).
//
//go:generate go run gen.go

// versionsJSON is the embedded copy of versions.json (the package-local mirror of
// the repo-root single source of truth; auto-derived from the root copy by
// `go generate` and kept in lockstep by drift_test.go).
//
//go:embed versions.json
var versionsJSON []byte

// Manifest is the parsed versions.json — the pins not already in go.mod.
type Manifest struct {
	// Go is the pinned Go toolchain version; drift_test.go asserts it equals the
	// `go` directive in go.mod.
	Go string `json:"go"`
	// Platforms is the set of supported build target os/arch pairs.
	Platforms []string `json:"platforms"`
	// ConductedTools pins the minimum versions of external tools the daemon and
	// integrator SHELL OUT to (e.g. git). doctor enforces these fail-closed.
	ConductedTools map[string]struct {
		Min string `json:"min"`
	} `json:"conducted_tools"`
}

// Load parses the embedded versions.json into a Manifest.
func Load() (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(versionsJSON, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse embedded versions.json: %w", err)
	}
	return m, nil
}

// GitVersion shells out to `git --version` and parses the "git version X.Y.Z"
// line down to the bare "X.Y.Z" version token.
func GitVersion() (string, error) {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("run git --version: %w", err)
	}
	// Output looks like "git version 2.39.5 (Apple Git-154)" — the version token
	// is the third whitespace-separated field; we tolerate vendor suffixes.
	fields := strings.Fields(string(out))
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return "", fmt.Errorf("unexpected git --version output: %q", strings.TrimSpace(string(out)))
	}
	return fields[2], nil
}

// CompareVersions does a numeric, dotted comparison of two version strings,
// returning -1 if a < b, 0 if equal, +1 if a > b. It is tolerant of missing
// trailing components (treated as 0) and of non-numeric suffixes on a component
// (e.g. "2.39.5-rc1" compares as 2.39.5): only the leading integer of each
// dotted component is significant.
func CompareVersions(a, b string) int {
	as := splitVersion(a)
	bs := splitVersion(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

// splitVersion turns "v2.38.0-rc1" into [2, 38, 0], stripping a leading "v",
// taking the leading integer of each dotted component and dropping any that have
// no leading digits.
func splitVersion(s string) []int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		out = append(out, leadingInt(p))
	}
	return out
}

// leadingInt parses the leading run of ASCII digits in p, returning 0 when none.
func leadingInt(p string) int {
	end := 0
	for end < len(p) && p[end] >= '0' && p[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(p[:end])
	if err != nil {
		return 0
	}
	return n
}

// CheckGit resolves the installed git version and reports whether it satisfies
// the given minimum (have >= min). A missing or unparseable git is returned as
// an error so callers can treat conducted-tool absence as fail-closed.
func CheckGit(min string) (have string, ok bool, err error) {
	have, err = GitVersion()
	if err != nil {
		return "", false, err
	}
	return have, CompareVersions(have, min) >= 0, nil
}

// keyModules are the dependencies whose versions are worth surfacing in
// `version -v` / doctor — the load-bearing ones: the cgo-free SQLite driver, the
// CLI framework, the TUI stack, the pty shim, and the terminal helper.
var keyModules = []string{
	"modernc.org/sqlite",
	"github.com/spf13/cobra",
	"github.com/charmbracelet/bubbletea",
	"github.com/creack/pty",
	"golang.org/x/term",
}

// ModuleVersions extracts the versions of keyModules from the binary's embedded
// build info (runtime/debug.ReadBuildInfo). Best effort: returns an empty (but
// non-nil) map when build info is unavailable (e.g. `go test` without modules)
// or a given module is absent.
func ModuleVersions() map[string]string {
	out := make(map[string]string, len(keyModules))
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	want := make(map[string]bool, len(keyModules))
	for _, m := range keyModules {
		want[m] = true
	}
	for _, dep := range bi.Deps {
		if dep == nil {
			continue
		}
		if want[dep.Path] {
			out[dep.Path] = dep.Version
		}
	}
	return out
}

// Render produces the `mad-substrate version` output. The non-verbose form is EXACTLY
// the historical single line (callers depend on its shape). The verbose form
// appends the embedded pins and module versions, each on its own indented line.
func Render(version, commit string, contractVersion int, verbose bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mad-substrate %s (commit %s, contract v%d)\n", version, commit, contractVersion)
	if !verbose {
		return b.String()
	}

	fmt.Fprintf(&b, "  platform:  %s/%s (running)\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "  go:        %s (running %s)\n", goPin(), runtime.Version())

	m, err := Load()
	if err == nil {
		fmt.Fprintf(&b, "  platforms: %s\n", strings.Join(m.Platforms, ", "))
		if g, gok := m.ConductedTools["git"]; gok {
			fmt.Fprintf(&b, "  git min:   %s\n", g.Min)
		}
	} else {
		fmt.Fprintf(&b, "  manifest:  unavailable (%v)\n", err)
	}

	mods := ModuleVersions()
	if len(mods) == 0 {
		fmt.Fprintf(&b, "  modules:   unavailable (no embedded build info)\n")
	} else {
		b.WriteString("  modules:\n")
		// Stable order: iterate keyModules so output is deterministic.
		for _, name := range keyModules {
			if v, vok := mods[name]; vok {
				fmt.Fprintf(&b, "    %s %s\n", name, v)
			}
		}
	}
	return b.String()
}

// goPin returns the pinned Go version from the embedded manifest, falling back to
// the empty string when the manifest cannot be loaded (Render handles that case).
func goPin() string {
	m, err := Load()
	if err != nil {
		return ""
	}
	return m.Go
}
