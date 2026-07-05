// Package singular implements project 7 (singular-gate): the default-deny
// boundary for resources with real external side effects.
//
// Owns (docs/0003 clause map): Inv 8 — the safety-property conjunct (c): no
// singular effect without an explicit grant. DENY is the STRUCTURAL ground state
// (the zero value of GrantMode), so absence/uncertainty/malformed-declaration all
// resolve to deny BY CONSTRUCTION — the cardinal rule is that a downward
// misclassification (treating a singular resource as reachable) is real-world
// damage, so every non-deny outcome is an explicit opt-in.
//
// Boundaries (what this is NOT): it does NOT decide what is singular (it CONSUMES
// the classifier's verdict — Inv 9), does NOT build its own lock (supervised
// grants serialize on the LEDGER's CAS over an opaque singular key), and does NOT
// write the child env (it PRODUCES the singular portion of the env-spec; the
// launcher applies it). Escape-resistance is structural: a denied/mock/proxy
// resource's env-spec never carries the REAL endpoint, so it is unreachable from
// the env-spec alone (Inv 4 — holds for an uncooperative agent that calls no
// adapter; full network confinement is the container/VM grain, honest-scope).
package singular

import (
	"fmt"
	"strings"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/manifest"
)

// GrantMode is the four-outcome grant decision. Deny is the ZERO value, so an
// unset/unknown mode is deny by construction (the load-bearing default-deny).
type GrantMode int

const (
	Deny       GrantMode = iota // ground state: no grant / unknown / malformed
	Mock                        // no real side effect
	Proxy                       // mad-trellis-mediated interposition (singular analog of the mediated remote)
	Supervised                  // real endpoint, reachable ONLY while holding the serialized grant
)

func (m GrantMode) String() string {
	switch m {
	case Mock:
		return "mock"
	case Proxy:
		return "proxy"
	case Supervised:
		return "supervised"
	default:
		return "deny" // total: any out-of-range value is deny (Inv 8 ground state)
	}
}

// LeaseGate is the supervised-grant serialization mechanism — the ledger's CAS,
// consumed (never reimplemented; the gate builds no lock of its own). A
// supervised grant is a lease over an opaque per-resource singular key; it
// auto-releases on holder death via the lease TTL (liveness reclaims it).
type LeaseGate interface {
	Acquire(key []byte, holder string, ttl time.Duration) (granted bool, currentHolder string, err error)
	Renew(key []byte, holder string, ttl time.Duration) (ok bool, err error)
	Release(key []byte, holder string) (bool, error)
}

// Classifier is the verdict source the gate CONSUMES to know a resource is
// singular (Inv 9 — it never reimplements classification).
type Classifier interface {
	Classify(ref manifest.ResourceRef) manifest.Kind
}

// AuditFunc emits a decision-audit record through the daemon's audit interface
// (nil → no-op). Keeping it a closure decouples this package from the daemon type.
type AuditFunc func(session, kind string, payload []byte)

// Gate is the default-deny singular-resource boundary.
type Gate struct {
	cls    Classifier
	grants map[string]manifest.Grant
	leases LeaseGate
	audit  AuditFunc
	ttl    time.Duration
}

// Options configures a Gate.
type Options struct {
	Classifier Classifier                // REQUIRED: the verdict source
	Grants     map[string]manifest.Grant // declared grants (nil → none → all deny)
	Leases     LeaseGate                 // REQUIRED: supervised-grant serialization
	Audit      AuditFunc                 // nil → no-op
	GrantTTLMs int64                     // supervised-grant lease TTL (default 120000)
}

// New constructs a Gate.
func New(opts Options) (*Gate, error) {
	if opts.Classifier == nil {
		return nil, fmt.Errorf("singular: a Classifier is required (the gate consumes the verdict)")
	}
	if opts.Leases == nil {
		return nil, fmt.Errorf("singular: a LeaseGate is required (supervised grants serialize on the ledger)")
	}
	grants := opts.Grants
	if grants == nil {
		grants = map[string]manifest.Grant{}
	}
	audit := opts.Audit
	if audit == nil {
		audit = func(string, string, []byte) {}
	}
	ttl := time.Duration(opts.GrantTTLMs) * time.Millisecond
	if ttl <= 0 {
		ttl = 120 * time.Second
	}
	return &Gate{cls: opts.Classifier, grants: grants, leases: opts.Leases, audit: audit, ttl: ttl}, nil
}

// Resolution is the PURE, deterministic grant decision for a resource — no lease
// is taken (that is Request's job). Singular reports whether the gate is even
// responsible (the classifier judged the resource singular).
type Resolution struct {
	Resource string
	Singular bool
	Mode     GrantMode
	Reason   string
}

// Resolve is the deterministic grant decision (Inv 2(b): no probabilistic
// component). A resource the classifier does NOT judge singular is not the gate's
// concern. A singular resource with no/unknown/malformed grant resolves to DENY.
func (g *Gate) Resolve(resource string) Resolution {
	if g.cls.Classify(manifest.ExternalRef(resource)) != manifest.Singular {
		return Resolution{Resource: resource, Singular: false, Mode: Deny, Reason: "not singular (gate not responsible)"}
	}
	grant, ok := g.grants[resource]
	if !ok {
		return Resolution{Resource: resource, Singular: true, Mode: Deny, Reason: "no grant declared (default-deny)"}
	}
	mode := normalizeMode(grant.Mode)
	reason := ""
	if mode == Deny {
		reason = fmt.Sprintf("unknown grant mode %q (default-deny)", grant.Mode)
	}
	// A supervised grant with NO endpoint is MALFORMED — it claims a reachable real
	// endpoint yet declares none. Inv 9: any ambiguity/malformed grant resolves to
	// the ground-state DENY (the gate is the authoritative denier), so it never
	// mints a "granted" supervised access nor takes the exclusive lease for a
	// non-functional grant. (Mock/Proxy route to sentinels and need no endpoint.)
	if mode == Supervised && strings.TrimSpace(grant.Endpoint) == "" {
		return Resolution{Resource: resource, Singular: true, Mode: Deny, Reason: "supervised grant has no endpoint (default-deny)"}
	}
	return Resolution{Resource: resource, Singular: true, Mode: mode, Reason: reason}
}

// Access is the env-spec routing produced for a singular resource. Env carries
// the singular portion the launcher applies. RealReachable reports whether Env
// exposes the REAL endpoint — TRUE only for a granted supervised resource; for
// deny/mock/proxy the real endpoint is NEVER present (proxy-bypass / Inv 8).
type Access struct {
	Resource      string
	Mode          GrantMode
	Granted       bool
	RealReachable bool
	Env           map[string]string
	Reason        string
}

// Request resolves the resource and produces its env-spec routing, acquiring the
// serialized grant lease for a supervised resource (CAS-fail-fast — a second
// supervised holder is denied, not queued). session is the daemon's
// connection-bound identity (Inv 4). The DENY path is the default and writes a
// non-routable sentinel — the real endpoint is unreachable from the env-spec.
func (g *Gate) Request(session, resource string) (Access, error) {
	r := g.Resolve(resource)
	name := envName(resource)
	if !r.Singular {
		// Not the gate's concern (forkable/convergent handled elsewhere). Emit no
		// singular env so the gate never shadows another mechanism's routing.
		return Access{Resource: resource, Mode: Deny, Granted: false, Reason: r.Reason}, nil
	}
	switch r.Mode {
	case Mock:
		g.audit(session, "singular.granted", payload(resource, "mock"))
		return Access{Resource: resource, Mode: Mock, Granted: true, RealReachable: false,
			Env: map[string]string{name: sentinel("mock", resource)}}, nil
	case Proxy:
		g.audit(session, "singular.granted", payload(resource, "proxy"))
		return Access{Resource: resource, Mode: Proxy, Granted: true, RealReachable: false,
			Env: map[string]string{name: sentinel("proxy", resource)}}, nil
	case Supervised:
		granted, holder, err := g.leases.Acquire(g.key(resource), session, g.ttl)
		if err != nil {
			return Access{}, err
		}
		if !granted {
			// The SAME session re-requesting its OWN live grant is a re-fetch, not a
			// denial: renew (extend) and return the real endpoint. A DIFFERENT session
			// is denied (≤1 holder) without disclosing the holder's opaque identity.
			if holder == session {
				if ok, rerr := g.leases.Renew(g.key(resource), session, g.ttl); rerr == nil && ok {
					g.audit(session, "singular.granted", payload(resource, "supervised-renew"))
					return Access{Resource: resource, Mode: Supervised, Granted: true, RealReachable: true,
						Env: map[string]string{name: g.grants[resource].Endpoint}}, nil
				}
			}
			g.audit(session, "singular.denied", payload(resource, "supervised-busy"))
			return Access{Resource: resource, Mode: Supervised, Granted: false, RealReachable: false,
				Env:    map[string]string{name: sentinel("denied", resource)},
				Reason: "supervised grant held by another session"}, nil
		}
		g.audit(session, "singular.granted", payload(resource, "supervised"))
		return Access{Resource: resource, Mode: Supervised, Granted: true, RealReachable: true,
			Env: map[string]string{name: g.grants[resource].Endpoint}}, nil
	default: // Deny
		g.audit(session, "singular.denied", payload(resource, "no-grant"))
		return Access{Resource: resource, Mode: Deny, Granted: false, RealReachable: false,
			Env: map[string]string{name: sentinel("denied", resource)}, Reason: r.Reason}, nil
	}
}

// Renew extends a supervised grant the caller holds — the heartbeat a LIVE holder
// uses so the lease TTL stays a DEATH detector (a live holder renews; a dead one
// does not, so liveness reclaims). Without periodic renewal a supervised holder
// whose work outlasts the TTL would lose exclusivity (chafe C16); the launcher /
// cooperative adapter is expected to heartbeat this. Holder-bound (Inv 4).
func (g *Gate) Renew(session, resource string) (bool, error) {
	return g.leases.Renew(g.key(resource), session, g.ttl)
}

// ReleaseSupervised releases a supervised grant held by session (idempotent).
func (g *Gate) ReleaseSupervised(session, resource string) (bool, error) {
	return g.leases.Release(g.key(resource), session)
}

// key is the opaque per-resource singular lease key (the gate owns the
// singular-resource→key mapping for supervised serialization; opaque to the
// ledger, disjoint from the trunk key namespace).
func (g *Gate) key(resource string) []byte {
	return []byte("mad-trellis:singular:v1:" + resource)
}

// normalizeMode maps a declared mode string to a GrantMode; anything unknown,
// empty, or malformed → Deny (the ground state).
func normalizeMode(s string) GrantMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "mock":
		return Mock
	case "proxy":
		return Proxy
	case "supervised":
		return Supervised
	default:
		return Deny
	}
}

// sentinel is a NON-ROUTABLE routing value for a non-real mode: a tool that honors
// the env fails closed, and the REAL endpoint string never appears, so it is
// unreachable from the env-spec alone (proxy-bypass escape-resistance).
func sentinel(kind, resource string) string {
	return "mad-trellis-" + kind + "://" + resource
}

func envName(resource string) string {
	var b strings.Builder
	b.WriteString("MAD_SINGULAR_")
	for _, r := range strings.ToUpper(resource) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func payload(resource, detail string) []byte {
	return []byte(fmt.Sprintf(`{"resource":%q,"detail":%q}`, resource, detail))
}
