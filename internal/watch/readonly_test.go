package watch

// READ-ONLY BY CONSTRUCTION (the cardinal rule for project 9a, Inv 13-readonly).
//
// This is a build-time grep/symbol test: it walks every NON-test .go source file
// in this package and FAILS if any FORBIDDEN mutating RPC method name string
// appears anywhere in the watch package. It is the structural guarantee that no
// code path — none — in the watch surface can reach a mutating daemon method.
//
// Positive control: it also asserts at least one ALLOWED read method string IS
// present, so the test can never be vacuously green (e.g. if the client were
// gutted).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenMethods is the exact list of MUTATING daemon RPC methods that must
// NEVER appear in internal/watch (from the 9a spec). If any of these strings is
// found in a non-test source file, the build of the watch surface is unsafe.
var forbiddenMethods = []string{
	"lease.acquire",
	"lease.renew",
	"lease.release",
	"integrate.submit",
	"integrate.promote",
	"integrate.abort",
	"substrate.provision",
	"substrate.teardown",
	"singular.resolve",
	"singular.request",
	"singular.renew",
	"singular.release",
	"audit.append",
	"liveness.scan",
}

// allowedReadMethods is the set of READ/QUERY methods the watch surface is
// permitted to call. At least one must appear (the non-vacuity control).
var allowedReadMethods = []string{
	"lease.list",
	"lease.inspect",
	"integrate.list",
	"integrate.status",
	"integrate.trunk",
	"integration.list",
	"audit.tail",
	"diag.health",
	"session.whoami",
}

// packageSources reads every NON-test .go file in the watch package directory
// (the directory containing this test). Robust to cwd: os.ReadDir(".") resolves
// to the package dir under `go test`.
func packageSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("no non-test source files found — grep test would be vacuous")
	}
	return out
}

func TestReadOnlyByConstruction_NoForbiddenMethods(t *testing.T) {
	sources := packageSources(t)
	for name, src := range sources {
		for _, forbidden := range forbiddenMethods {
			if strings.Contains(src, forbidden) {
				t.Errorf("FORBIDDEN mutating RPC method %q appears in %s — the watch surface MUST be read-only by construction (Inv 13-readonly)", forbidden, name)
			}
		}
	}
}

func TestReadOnlyByConstruction_PositiveControl(t *testing.T) {
	sources := packageSources(t)
	var all strings.Builder
	for _, src := range sources {
		all.WriteString(src)
	}
	combined := all.String()

	found := false
	for _, allowed := range allowedReadMethods {
		if strings.Contains(combined, allowed) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("non-vacuity control FAILED: not one of the ALLOWED read methods %v appears in the watch package — the grep test would pass vacuously even if the client were gutted", allowedReadMethods)
	}
}

// TestForbiddenListIsNonEmpty guards the guard: if the forbidden list were ever
// emptied, the negative test above would silently pass on anything.
func TestForbiddenListIsNonEmpty(t *testing.T) {
	if len(forbiddenMethods) == 0 {
		t.Fatal("forbiddenMethods is empty — the read-only grep test would be vacuous")
	}
}
