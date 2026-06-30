package buildinfo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// drift_test.go is the C18 guard: the version pins live in THREE places that MUST
// agree — the embedded versions.json (this package), the canonical repo-root
// versions.json, and the `go` directive in go.mod. This untagged test runs in the
// normal `go test ./...` sweep so any drift fails CI loudly rather than surfacing
// as a confusing runtime mismatch.

// repoRoot walks up from this test file's directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod walking up from %s", wd)
		}
		dir = parent
	}
}

// parseGoDirective extracts the version from the `go X.Y.Z` directive in go.mod.
func parseGoDirective(t *testing.T, gomod string) string {
	t.Helper()
	b, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			return fields[1]
		}
	}
	t.Fatalf("no `go` directive found in %s", gomod)
	return ""
}

func TestEmbeddedManifestMatchesRoot(t *testing.T) {
	root := repoRoot(t)
	rootBytes, err := os.ReadFile(filepath.Join(root, "versions.json"))
	if err != nil {
		t.Fatalf("read root versions.json: %v", err)
	}
	// Compare semantically (whitespace-insensitive) so formatting differences in
	// the two copies don't trip the guard — only the pinned VALUES must match.
	var rootM, embeddedM Manifest
	if err := json.Unmarshal(rootBytes, &rootM); err != nil {
		t.Fatalf("parse root versions.json: %v", err)
	}
	embeddedM, err = Load()
	if err != nil {
		t.Fatalf("Load embedded: %v", err)
	}
	// DeepEqual the WHOLE parsed manifest so EVERY field is guarded — including the
	// full conducted_tools map (not just the "git" key) and any field added later;
	// a value-level (not byte-level) compare so formatting differences between the
	// two copies don't trip the guard.
	if !reflect.DeepEqual(rootM, embeddedM) {
		t.Errorf("versions.json drift between root and embedded copies (run `go generate ./internal/buildinfo/...`):\n  root:     %+v\n  embedded: %+v", rootM, embeddedM)
	}
}

func TestManifestGoMatchesGoMod(t *testing.T) {
	root := repoRoot(t)
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	goDirective := parseGoDirective(t, filepath.Join(root, "go.mod"))
	if m.Go != goDirective {
		t.Errorf("versions.json go=%q != go.mod go directive %q", m.Go, goDirective)
	}
}
