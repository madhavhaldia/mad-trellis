// Package manifest implements project 3 (manifest-classifier): the per-repo
// declaration and the deterministic classify-upward engine that routes every
// resource to its mechanism (isolate / lease+integrate / gate).
//
// Owns (docs/0003 clause map): Inv 9 (classify upward under uncertainty — the
// soul), Inv 11 (the loader READS a declaration; init writes ONLY the manifest
// and never modifies project files), and its local 2(b) slice (classify is a
// pure, deterministic, TOTAL function — no probabilistic/heuristic/LLM
// component, ever).
//
// Domain-aware defaults (docs/0004 card 3): a working-tree PATH defaults to
// Forkable (the worktree gives a copy; the trunk lease + integrator catch any
// merge hazard, so a misclassified path is at worst a caught conflict, never
// silent corruption), while an EXTERNAL resource defaults to Singular
// (default-deny, Inv 8 — a real side effect has no integrator backstop).
// Convergent routing is domain-aware: the trunk domain routes to the global
// trunk-merge lease (TrunkKey), while a convergent PATH routes to a per-path key
// and a convergent EXTERNAL resource routes to a per-name key, so distinct
// convergent resources get distinct leases (real per-resource claims).
package manifest

import (
	"path/filepath"
	"strings"
)

// Kind is the resource classification. Strictness increases with the value:
// Forkable < Convergent < Singular.
type Kind int

const (
	Forkable Kind = iota
	Convergent
	Singular
)

func (k Kind) String() string {
	switch k {
	case Forkable:
		return "forkable"
	case Convergent:
		return "convergent"
	case Singular:
		return "singular"
	default:
		return "singular" // total: any out-of-range value is treated strictest
	}
}

// Valid reports whether k is a defined kind.
func (k Kind) Valid() bool { return k == Forkable || k == Convergent || k == Singular }

func stricter(a, b Kind) Kind {
	if a >= b {
		return a
	}
	return b
}

// Domain is the namespace of a resource reference.
type Domain int

const (
	DomainPath     Domain = iota // a working-tree path
	DomainExternal               // a named external/side-effecting resource
	DomainTrunk                  // the integration target (always convergent)
)

// ResourceRef identifies a resource to classify.
type ResourceRef struct {
	Domain Domain
	Name   string // path for DomainPath; resource name for DomainExternal; ignored for DomainTrunk
}

// PathRef, ExternalRef, and TrunkRef are convenience constructors.
func PathRef(p string) ResourceRef     { return ResourceRef{Domain: DomainPath, Name: p} }
func ExternalRef(n string) ResourceRef { return ResourceRef{Domain: DomainExternal, Name: n} }
func TrunkRef() ResourceRef            { return ResourceRef{Domain: DomainTrunk} }

// LeaseKey is an opaque lease key (consumed by the ledger; never parsed there).
type LeaseKey []byte

// TrunkKey is the global TRUNK-MERGE lease key. It serializes trunk writes (the
// conductor/integrator hold it for the single atomic trunk CAS, Inv 7) and is
// the key for the trunk domain. Convergent PATHS and convergent EXTERNAL
// resources no longer collapse onto it — they route to per-resource keys (see
// convergentPathKey / convergentExternalKey) so two agents on distinct
// convergent resources no longer falsely serialize.
//
// R5 (advisory coordination, not parallel merges): per-path / per-key convergent
// leases are COORDINATION only. They let DISTINCT convergent resources avoid
// false serialization and let `mad_locks` display WHICH resource is held.
// They do NOT make trunk integration parallel: the trunk WRITE itself is serial
// by necessity (one ref, one atomic CAS under TrunkKey, Inv 7), so two agents
// converging onto trunk still serialize at the merge — these keys are not
// parallel merges and must not be mis-sold as such.
var TrunkKey = LeaseKey("mad-trellis:trunk:v1")

// convergentKeyPrefix namespaces per-resource convergent lease keys so `locks`
// can recover the resource for display and they never collide with the
// trunk-merge key. externalConvergentKeyPrefix further namespaces EXTERNAL
// convergent keys under "external:" so a per-name external key is never mistaken
// for a per-path key (and vice versa).
const (
	convergentKeyPrefix         = "mad-trellis:convergent:v1:"
	externalConvergentKeyPrefix = convergentKeyPrefix + "external:"
)

// convergentPathKey encodes the path in the key so two agents on DISTINCT
// convergent paths get DISTINCT leases (real per-file claims) and `locks` can
// recover the path for display. The key is daemon-issued (clients never craft
// claim keys), so encoding the path here is safe; the merge-tree gate — not key
// opacity — is the safety boundary.
func convergentPathKey(path string) LeaseKey { return LeaseKey(convergentKeyPrefix + path) }

// convergentExternalKey encodes the external resource name in the key (under the
// "external:" namespace) so two agents on DISTINCT convergent external resources
// get DISTINCT leases instead of collapsing onto the global trunk key. Same
// rationale as convergentPathKey: daemon-issued, display-oriented; the gate is
// the safety boundary, not key opacity.
func convergentExternalKey(name string) LeaseKey { return LeaseKey(externalConvergentKeyPrefix + name) }

// PathFromConvergentKey returns (path, true) if key is a per-path convergent key
// minted by convergentPathKey, so a display surface (e.g. `locks`) can show which
// file is held rather than an opaque key. External convergent keys (which share
// the convergentKeyPrefix but add the "external:" namespace) are NOT paths and
// are excluded — use ExternalFromConvergentKey for those.
func PathFromConvergentKey(key []byte) (string, bool) {
	s := string(key)
	if strings.HasPrefix(s, externalConvergentKeyPrefix) {
		return "", false // an external per-name key, not a path
	}
	if strings.HasPrefix(s, convergentKeyPrefix) {
		return strings.TrimPrefix(s, convergentKeyPrefix), true
	}
	return "", false
}

// ExternalFromConvergentKey returns (name, true) if key is a per-name external
// convergent key minted by convergentExternalKey, so a display surface can show
// which external resource is held rather than an opaque key.
func ExternalFromConvergentKey(key []byte) (string, bool) {
	s := string(key)
	if strings.HasPrefix(s, externalConvergentKeyPrefix) {
		return strings.TrimPrefix(s, externalConvergentKeyPrefix), true
	}
	return "", false
}

// Classifier is the deterministic classify-upward engine over a loaded Manifest.
type Classifier struct{ m *Manifest }

// New returns a Classifier over the given manifest (nil → defaults).
func New(m *Manifest) *Classifier {
	if m == nil {
		m = DefaultManifest()
	}
	return &Classifier{m: m}
}

// Classify is the TOTAL classify-upward function: every input resolves to a
// valid Kind; under any uncertainty it resolves to the stricter kind.
func (c *Classifier) Classify(ref ResourceRef) Kind {
	switch ref.Domain {
	case DomainTrunk:
		return Convergent
	case DomainPath:
		switch {
		case matchAny(c.m.SingularPaths, ref.Name): // declared singular, or conflicting → strictest
			return Singular
		case matchAny(c.m.ConvergentPaths, ref.Name):
			return Convergent
		default:
			return Forkable // default: worktree copy; trunk-lease backstop
		}
	case DomainExternal:
		var decls []Kind
		if c.m.ForkableResources[ref.Name] {
			decls = append(decls, Forkable)
		}
		if c.m.ConvergentResources[ref.Name] {
			decls = append(decls, Convergent)
		}
		if c.m.SingularResources[ref.Name] {
			decls = append(decls, Singular)
		}
		switch len(decls) {
		case 0:
			return Singular // silence → default-deny (Inv 8 + classify-upward)
		case 1:
			return decls[0]
		default:
			k := decls[0]
			for _, d := range decls[1:] {
				k = stricter(k, d)
			}
			return k // conflicting declarations → strictest
		}
	default:
		return Singular // unknown domain → strictest (total; never abstains)
	}
}

// Route returns the kind and, for Convergent, the lease key. Convergent routing
// is domain-aware: the TRUNK domain gets the global TrunkKey (the trunk-merge
// lease the conductor/integrator serialize on, Inv 7), while a convergent PATH
// gets a per-path key and a convergent EXTERNAL resource gets a per-name key
// (namespaced under "external:") so distinct convergent resources no longer
// share one lock. R5: these per-resource keys are advisory COORDINATION (avoid
// false serialization + name the held resource for `locks`), NOT parallel merges
// — the trunk write under TrunkKey stays serial (see TrunkKey).
func (c *Classifier) Route(ref ResourceRef) (Kind, LeaseKey) {
	k := c.Classify(ref)
	if k != Convergent {
		return k, nil
	}
	switch ref.Domain {
	case DomainTrunk:
		return k, TrunkKey // global merge lease — unchanged
	case DomainPath:
		return k, convergentPathKey(ref.Name) // per-path, path-encoded
	default:
		return k, convergentExternalKey(ref.Name) // per-name external, "external:"-namespaced
	}
}

func matchAny(globs []string, path string) bool {
	for _, g := range globs {
		if matchGlob(g, path) {
			return true
		}
	}
	return false
}

// matchGlob reports whether path matches a glob supporting `*` (within a path
// segment, filepath.Match semantics) and `**` (zero or more whole segments).
// Matching is exact and deterministic — no fuzzy/heuristic logic (Inv 2(b)).
func matchGlob(pattern, path string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true // trailing ** matches any remaining segments
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := filepath.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
