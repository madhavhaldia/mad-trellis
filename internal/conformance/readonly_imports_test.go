package conformance

// readonly_imports_test.go is the BLACK-BOX grep test (ship-time, untagged so it
// runs on every `go test`): the conformance package must import NONE of the
// forbidden internal packages — it asserts safety through the PUBLIC surface only
// (the daemon contract + CLI + observable state), never by reaching into another
// project's internals. The ONLY internal packages it may import are
// internal/rpcclient (the wire client — speaks the frozen public protocol, like
// any external client) and internal/protocol (envelope/taxonomy types).
//
// This is the structural guarantee behind "verify through the right layer"
// (docs/0004 §10a top risk): if a future probe-writer reaches into
// internal/lease (etc.) to peek at state, this test goes RED at ship time.
//
// POSITIVE CONTROL: the SAME dependency scan must CONFIRM the package DOES import
// rpcclient (and the package list is non-empty) — so a vacuous "it imports
// nothing" can never pass the allowlist half.

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	conformancePkg = "github.com/madhavhaldia/mad-substrate/internal/conformance"
	modulePrefix   = "github.com/madhavhaldia/mad-substrate/"
	internalPrefix = modulePrefix + "internal/"
)

// allowedInternal is the ONLY internal coupling permitted: the wire client + the
// protocol envelope types (both speak the frozen public contract), and the
// conformance package itself. This is an ALLOWLIST (fix #2): rather than blocking
// a hardcoded list of forbidden package NAMES (which a NEW internal package would
// slip past), we assert that EVERY dependency under the module's internal/ prefix
// is one of these — so any other internal import, named or not, is caught.
var allowedInternal = map[string]bool{
	modulePrefix + "internal/rpcclient":   true,
	modulePrefix + "internal/protocol":    true,
	modulePrefix + "internal/conformance": true,
}

// packageImports returns the TEST-INCLUSIVE transitive dependency graph of the
// conformance package. The `-test` flag (fix #1) folds in the deps of the
// package's _test.go files, so a forbidden internal import buried in a test file
// (e.g. a zz_inject_test.go importing internal/lease) can no longer EVADE the
// guard — the prior `-deps` without `-test` scanned only the non-test build graph.
func packageImports(t *testing.T) []string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	// `go list -deps -test` prints the full transitive dependency set INCLUDING the
	// package's test files — the build-graph truth, so an import buried under a
	// helper OR a _test.go cannot hide.
	out, err := exec.Command(goBin, "list", "-deps", "-test", conformancePkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps -test %s: %v\n%s", conformancePkg, err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// internalDeps returns every dependency under the module's internal/ prefix,
// normalizing the `[…]` / `.test` synthetic package suffixes `go list -test`
// appends so the bare import path is matched.
func internalDeps(deps []string) []string {
	var out []string
	for _, d := range deps {
		d = strings.TrimSpace(d)
		// `go list -test` emits synthetic package ids like
		// "pkg [pkg.test]" and "pkg.test" — strip the bracketed variant suffix and the
		// ".test" synthetic test-main package so we match the underlying import path.
		if i := strings.IndexByte(d, ' '); i >= 0 {
			d = d[:i]
		}
		d = strings.TrimSuffix(d, ".test")
		if strings.HasPrefix(d, internalPrefix) {
			out = append(out, d)
		}
	}
	return out
}

// TestConformanceImportsOnlyAllowlistedInternal is the ALLOWLIST guard (fix #1+#2):
// EVERY internal/* dependency of the conformance package (test-inclusive) must be
// in the small allowed set {rpcclient, protocol, conformance}. A NEW internal
// package, or a forbidden import slipped into a _test.go, is caught here — the
// package must assert through the PUBLIC surface, never another project's internals.
func TestConformanceImportsOnlyAllowlistedInternal(t *testing.T) {
	deps := internalDeps(packageImports(t))
	for _, d := range deps {
		if !allowedInternal[d] {
			t.Errorf("CARDINAL RULE VIOLATED: conformance (test-inclusive) imports internal package %q "+
				"which is NOT on the allowlist {rpcclient, protocol} — assert through the PUBLIC surface, "+
				"not another project's internals", strings.TrimPrefix(d, modulePrefix))
		}
	}
}

// TestConformanceImportsWireClient is the POSITIVE CONTROL: the package DOES
// import the allowed wire client + protocol — so the allowlist check above is
// non-vacuous (it is not passing merely because the scan found no internal imports
// at all, which would make "all internal deps are allowlisted" trivially true).
func TestConformanceImportsWireClient(t *testing.T) {
	deps := packageImports(t)
	depSet := map[string]bool{}
	for _, d := range internalDeps(deps) {
		depSet[d] = true
	}
	if len(deps) < 2 {
		t.Fatalf("dependency scan returned implausibly few deps (%d) — the scan itself may be broken", len(deps))
	}
	for _, a := range []string{"internal/rpcclient", "internal/protocol"} {
		full := modulePrefix + a
		if !depSet[full] {
			t.Errorf("CONTROL VACUOUS: conformance does NOT import the allowed wire package %q — "+
				"the allowlist check cannot be trusted (the package must reach the daemon "+
				"through the public protocol)", a)
		}
	}
}
