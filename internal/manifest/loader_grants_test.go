package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// The emergent grants extension parses per-singular-resource grant declarations
// (project 7). It is tolerant: an absent grants block yields an empty map, and a
// grant with no resource is skipped. The loader carries the declaration verbatim;
// the gate decides deny for an unknown mode (so unknown modes still parse).
func TestLoadGrants(t *testing.T) {
	dir := t.TempDir()
	body := `{
      "version": 1,
      "singular": {
        "resources": ["stripe", "prod-db"],
        "grants": [
          {"resource": "stripe", "mode": "mock"},
          {"resource": "prod-db", "mode": "supervised", "endpoint": "postgres://prod/main"},
          {"resource": "weird", "mode": "totally-unknown"},
          {"mode": "mock"}
        ]
      }
    }`
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if g := m.Grants["stripe"]; g.Mode != "mock" {
		t.Fatalf("stripe grant mode: got %q", g.Mode)
	}
	if g := m.Grants["prod-db"]; g.Mode != "supervised" || g.Endpoint != "postgres://prod/main" {
		t.Fatalf("prod-db grant: got %+v", g)
	}
	if g := m.Grants["weird"]; g.Mode != "totally-unknown" {
		t.Fatalf("an unknown mode must still parse verbatim (the gate denies it); got %q", g.Mode)
	}
	if _, ok := m.Grants[""]; ok {
		t.Fatal("a grant with no resource must be skipped")
	}
}

func TestLoadNoGrantsIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Grants == nil || len(m.Grants) != 0 {
		t.Fatalf("absent grants must yield an empty (non-nil) map; got %+v", m.Grants)
	}
	// The default manifest (no file) also has an empty grants map.
	if DefaultManifest().Grants == nil {
		t.Fatal("DefaultManifest must initialize Grants")
	}
}
