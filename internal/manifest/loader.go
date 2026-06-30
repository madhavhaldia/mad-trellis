package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ManifestFile is the per-repo declaration. v1 uses JSON to stay within the
// stdlib-only v1 dependency set (docs/0002 forbids a TOML lib in v1); the
// format is emergent behind this loader and may swap later without touching
// consumers (the Classifier interface is the stable contract, not the schema).
const ManifestFile = "mad-substrate.json"

// SupportedVersion is the manifest schema version this build understands.
const SupportedVersion = 1

// Grant is an OPTIONAL handling declaration for a SINGULAR resource (project 7,
// singular-gate): without a grant a singular resource is default-DENIED. The
// schema is emergent; this is the v1 minimal shape. Mode is "mock" | "proxy" |
// "supervised" (any other / empty value is treated as DENY by the gate — the gate
// owns the decision; the loader only carries the declaration). Endpoint is the
// real resource location the gate routes to under proxy/supervised; it is never
// surfaced to a denied or mocked resource.
type Grant struct {
	Mode     string
	Endpoint string
}

// Manifest is the loaded, in-memory declaration — the stable read-model.
type Manifest struct {
	Version             int
	ConvergentPaths     []string
	SingularPaths       []string
	ForkableResources   map[string]bool
	ConvergentResources map[string]bool
	SingularResources   map[string]bool
	Grants              map[string]Grant // resource name -> grant (absent => default-deny)
	ConductorEnabled    bool             // run automatic convergence on stream completion (default true)
	ConductorGate       string           // shell gate command run before merge; "" => merge-only
}

// DefaultManifest is the policy when nothing is declared: the working tree is
// forkable (no convergent/singular paths) and every external resource is
// singular by default (none declared forkable), with NO grants (default-deny).
func DefaultManifest() *Manifest {
	return &Manifest{
		Version:             SupportedVersion,
		ForkableResources:   map[string]bool{},
		ConvergentResources: map[string]bool{},
		SingularResources:   map[string]bool{},
		Grants:              map[string]Grant{},
		// Conductor (automatic convergence) is ON by default — continuous
		// convergence — and merge-only (no gate) until a gate is declared.
		ConductorEnabled: true,
	}
}

type rawManifest struct {
	Version  int `json:"version"`
	Forkable struct {
		Resources []string `json:"resources"`
	} `json:"forkable"`
	Convergent struct {
		Paths     []string `json:"paths"`
		Resources []string `json:"resources"`
	} `json:"convergent"`
	Singular struct {
		Paths     []string `json:"paths"`
		Resources []string `json:"resources"`
		Grants    []struct {
			Resource string `json:"resource"`
			Mode     string `json:"mode"`
			Endpoint string `json:"endpoint"`
		} `json:"grants"`
	} `json:"singular"`
	Conductor *struct {
		Enabled *bool  `json:"enabled"`
		Gate    string `json:"gate"`
	} `json:"conductor"`
}

// Load reads the manifest at repoRoot. A missing manifest yields
// DefaultManifest (tolerant). Malformed JSON is an error (fail-closed — do not
// run on a broken policy). Unknown fields are ignored; an unknown version is
// accepted forward-tolerantly using the fields this build understands.
func Load(repoRoot string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ManifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultManifest(), nil
	}
	if err != nil {
		return nil, err
	}
	var raw rawManifest
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", ManifestFile, err)
	}
	return manifestFromRaw(raw), nil
}

func manifestFromRaw(raw rawManifest) *Manifest {
	m := DefaultManifest()
	if raw.Version != 0 {
		m.Version = raw.Version
	}
	m.ConvergentPaths = raw.Convergent.Paths
	m.SingularPaths = raw.Singular.Paths
	for _, r := range raw.Forkable.Resources {
		m.ForkableResources[r] = true
	}
	for _, r := range raw.Convergent.Resources {
		m.ConvergentResources[r] = true
	}
	for _, r := range raw.Singular.Resources {
		m.SingularResources[r] = true
	}
	// Grants are declared per singular resource; an empty/unknown mode is left
	// verbatim for the gate to treat as deny (the loader carries the declaration,
	// it does not decide). A grant for an unknown resource is harmless (the gate
	// only consults it after the classifier judges the resource singular).
	for _, gnt := range raw.Singular.Grants {
		if gnt.Resource == "" {
			continue
		}
		m.Grants[gnt.Resource] = Grant{Mode: gnt.Mode, Endpoint: gnt.Endpoint}
	}
	// Conductor: absent block leaves the DefaultManifest policy (enabled, no
	// gate). A present block with `enabled` omitted keeps enabled default-on; a
	// pointer distinguishes "absent" from an explicit `false`.
	if raw.Conductor != nil {
		if raw.Conductor.Enabled != nil {
			m.ConductorEnabled = *raw.Conductor.Enabled
		}
		m.ConductorGate = raw.Conductor.Gate
	}
	return m
}

// defaultScaffold is written by Init: it pre-declares common convergent hazards
// and leaves the resource lists for the project to fill.
const defaultScaffold = `{
  "version": 1,
  "conductor": { "enabled": true, "gate": "" },
  "forkable": { "resources": [] },
  "convergent": { "paths": ["migrations/**", "**/*.lock"], "resources": [] },
  "singular": { "paths": [], "resources": [] }
}
`

// Init scaffolds the manifest at repoRoot if absent, writing ONLY that file and
// touching nothing else (Inv 11 — declaration, never modification). It is
// idempotent: created=false when the manifest already exists.
func Init(repoRoot string) (created bool, err error) {
	path := filepath.Join(repoRoot, ManifestFile)
	switch _, statErr := os.Stat(path); {
	case statErr == nil:
		return false, nil
	case !errors.Is(statErr, fs.ErrNotExist):
		return false, statErr
	}
	if err := os.WriteFile(path, []byte(defaultScaffold), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
