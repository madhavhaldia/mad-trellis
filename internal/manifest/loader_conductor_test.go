package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// The conductor (automatic-convergence) extension parses conductor.{enabled,gate}.
// enabled is default-ON (continuous convergence) — a pointer in the raw schema
// distinguishes an absent block (default holds) from an explicit false. gate
// defaults to "" (merge-only).
func TestLoadConductor(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantEnabled bool
		wantGate    string
	}{
		{
			name:        "explicit disabled with gate",
			body:        `{"version":1,"conductor":{"enabled":false,"gate":"make test"}}`,
			wantEnabled: false,
			wantGate:    "make test",
		},
		{
			name:        "absent block defaults on, no gate",
			body:        `{"version":1}`,
			wantEnabled: true,
			wantGate:    "",
		},
		{
			name:        "enabled omitted holds default on, gate set",
			body:        `{"version":1,"conductor":{"gate":"just gate"}}`,
			wantEnabled: true,
			wantGate:    "just gate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			m, err := Load(dir)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if m.ConductorEnabled != tc.wantEnabled {
				t.Errorf("ConductorEnabled: got %v, want %v", m.ConductorEnabled, tc.wantEnabled)
			}
			if m.ConductorGate != tc.wantGate {
				t.Errorf("ConductorGate: got %q, want %q", m.ConductorGate, tc.wantGate)
			}
		})
	}
}

func TestDefaultManifestConductorOn(t *testing.T) {
	if !DefaultManifest().ConductorEnabled {
		t.Fatal("DefaultManifest must default ConductorEnabled true (conductor is continuous by default)")
	}
	if g := DefaultManifest().ConductorGate; g != "" {
		t.Fatalf("DefaultManifest ConductorGate must be empty (merge-only); got %q", g)
	}
}

// The Init scaffold must round-trip: re-loading the written file yields the
// default policy (conductor on, no gate).
func TestInitScaffoldConductor(t *testing.T) {
	dir := t.TempDir()
	created, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("Init must create the scaffold in an empty dir")
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("load scaffold: %v", err)
	}
	if !m.ConductorEnabled {
		t.Error("scaffold must yield ConductorEnabled true")
	}
	if m.ConductorGate != "" {
		t.Errorf("scaffold ConductorGate must be empty; got %q", m.ConductorGate)
	}
}
