// Package substrate implements project 4 (isolation-substrate): it constructs
// the per-agent FORKABLE boundaries before an agent runs and returns an
// immutable EnvSpec the launcher applies.
//
// Owns (docs/0003 clause map): Inv 1 (isolation of forkable FS + runtime + ports
// + local-state) and Inv 10-grainswap (the worktree→container→VM dial); it
// discharges the Inv-4 "sandbox-harder" tail at the achievable worktree grain
// (see contain.go for the honest scope and the seams that carry the rest).
//
// Boundaries (what this package is NOT): it produces a SPEC — it never execs or
// attaches a PTY (launcher, project 5), never redirects the git remote
// (integrator, project 6), and never routes a singular resource (gate, project
// 7). And critically (Inv 2(b)): WHAT to isolate is the CLASSIFIER's verdict,
// never the substrate's own policy — only a Forkable resource is materialized
// into the per-agent env; a Convergent/Singular (or silent→singular) resource is
// recorded as deferred and never auto-forked.
package substrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/madhavhaldia/mad-substrate/internal/manifest"
)

// Substrate constructs and tracks per-agent forkable boundaries. It is hosted in
// the single arbiter daemon (Inv 5), so its in-memory port allocator and live
// registry need no cross-process coordination.
type Substrate struct {
	repoAbs      string
	grain        Grain
	cls          *manifest.Classifier
	ports        *portAllocator
	defaultPorts int

	mu        sync.Mutex
	live      map[string]*EnvSpec // slug -> completed boundary
	reserving map[string]struct{} // slug -> a provision in flight (reserve-first)
}

// Options configures a Substrate.
type Options struct {
	Grain Grain // an explicit grain (wins over the dial); nil → select via GrainName/env
	// GrainName selects the grain by name ("worktree" | "container") when Grain is
	// nil. Empty → the MAD_GRAIN env, then the DEFAULT worktree grain. The
	// dial is the only behavior switch; everything composed OVER the grain is
	// grain-agnostic (Inv 10-grainswap).
	GrainName    string
	DefaultPorts int // ports allocated per agent (default 4)
}

// New constructs a Substrate over repo, gated by the classifier (nil → defaults).
func New(repo string, cls *manifest.Classifier, opts Options) (*Substrate, error) {
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	if cls == nil {
		cls = manifest.New(nil)
	}
	// Grain DIAL (Inv 10-grainswap). An explicit Options.Grain wins; otherwise the
	// dial is read from Options.GrainName, falling back to the MAD_GRAIN env,
	// and the DEFAULT stays the v1 worktree grain (zero behavior change unless the
	// container grain is explicitly selected).
	g := opts.Grain
	if g == nil {
		dial := strings.TrimSpace(opts.GrainName)
		if dial == "" {
			dial = strings.TrimSpace(os.Getenv("MAD_GRAIN"))
		}
		switch strings.ToLower(dial) {
		case "container":
			g = newContainerGrain(repoAbs)
		case "", "worktree":
			g = &worktreeGrain{repo: repoAbs}
		default:
			return nil, fmt.Errorf("substrate: unknown grain %q (want worktree|container)", dial)
		}
	}
	dp := opts.DefaultPorts
	if dp <= 0 {
		dp = 4
	}
	return &Substrate{
		repoAbs:      repoAbs,
		grain:        g,
		cls:          cls,
		ports:        newPortAllocator(),
		defaultPorts: dp,
		live:         map[string]*EnvSpec{},
		reserving:    map[string]struct{}{},
	}, nil
}

// Request parameterizes a provision.
type Request struct {
	Ports     int           // ports per agent (0 → the substrate default)
	Resources []ResourceReq // declared resources, each routed by classifier verdict
}

// ResourceReq is a declared resource the agent's env references. The substrate
// classifies it and isolates it ONLY if Forkable.
type ResourceReq struct {
	Name   string // env var to inject if forkable ("" → derived from Ref)
	Domain string // classifier domain: "path" | "external"
	Ref    string // the resource ref (a path, or an external resource name)
}

// shouldIsolate is the WHOLE isolation policy: isolate iff the classifier judged
// the resource Forkable. Deterministic, no inference (Inv 2(b)).
func shouldIsolate(k manifest.Kind) bool { return k == manifest.Forkable }

// Provision builds the forkable boundary for session and returns its immutable
// EnvSpec. session is the daemon's connection-bound identity (Inv 4): the caller
// (the RPC handler) passes cc.Session, never a value from params. On any partial
// failure the boundary is fully rolled back, so a failed provision leaks nothing.
func (s *Substrate) Provision(session string, req Request) (spec *EnvSpec, err error) {
	if strings.TrimSpace(session) == "" {
		return nil, fmt.Errorf("substrate: empty session")
	}
	slug := safeSlug(session)
	if slug == "" {
		return nil, fmt.Errorf("substrate: session has no safe slug: %q", session)
	}

	// Reserve the slug BEFORE any allocation (Inv 1 atomicity under the grain
	// dial): a second concurrent Provision of the same session is rejected here,
	// before it can build aliased slug-derived paths whose rollback would destroy
	// the winner's LIVE boundary at an idempotent grain. The live map and the
	// reservation set are keyed by SLUG (not the raw session), so two distinct
	// sessions that sanitize to one slug also collapse to a single boundary rather
	// than racing over shared paths.
	s.mu.Lock()
	if _, dup := s.live[slug]; dup {
		s.mu.Unlock()
		return nil, fmt.Errorf("substrate: session %q already provisioned", session)
	}
	if _, busy := s.reserving[slug]; busy {
		s.mu.Unlock()
		return nil, fmt.Errorf("substrate: session %q busy: a lifecycle operation is in flight", session)
	}
	s.reserving[slug] = struct{}{}
	s.mu.Unlock()

	nports := req.Ports
	if nports <= 0 {
		nports = s.defaultPorts
	}

	var (
		boundary  Boundary
		ports     []int
		agentRoot string
		built     bool
	)
	// Rollback: until `built` flips true, undo everything we constructed so a
	// failed provision is atomic (no orphan worktree, ports, or state dirs).
	defer func() {
		if built {
			return
		}
		s.mu.Lock()
		delete(s.reserving, slug) // release the reservation so the slug is usable again
		s.mu.Unlock()
		// The slug was reserved for the whole construction, so these paths are
		// private to THIS call — undoing them cannot touch another live boundary.
		if len(ports) > 0 {
			s.ports.release(ports)
		}
		if agentRoot != "" {
			_ = removeState(agentRoot)
		}
		if boundary.Cwd != "" {
			_ = s.grain.Teardown(boundary)
		}
	}()

	boundary, err = s.grain.Provision(slug)
	if err != nil {
		return nil, fmt.Errorf("substrate: grain provision: %w", err)
	}
	// The boundary must live OUTSIDE the governed repo (chafe C2): nesting it
	// would dirty the repo's `git status` / touch a project file. Enforce it
	// structurally, not just by doc claim — a misconfigured grain/env fails closed.
	// Check the HOST-side worktree (at the container grain Cwd is the in-container
	// mount point /work — a non-host path — while HostWorktree is the real host dir
	// the disjointness property is about). Fall back to Cwd for grains that report
	// only a host Cwd.
	hostPath := boundary.HostWorktree
	if hostPath == "" {
		hostPath = boundary.Cwd
	}
	if !disjoint(hostPath, s.repoAbs) {
		return nil, fmt.Errorf("substrate: grain placed boundary %q inside the governed repo %q", hostPath, s.repoAbs)
	}
	ports, err = s.ports.allocate(nports)
	if err != nil {
		return nil, fmt.Errorf("substrate: port allocate: %w", err)
	}
	var stateDirs map[string]string
	stateDirs, agentRoot, err = provisionState(s.repoAbs, slug)
	if err != nil {
		return nil, fmt.Errorf("substrate: local-state: %w", err)
	}
	if !disjoint(agentRoot, s.repoAbs) {
		return nil, fmt.Errorf("substrate: local-state root %q inside the governed repo %q", agentRoot, s.repoAbs)
	}

	env := s.buildEnv(session, ports, stateDirs)

	// Classifier-gated resource routing (Inv 2(b)): isolate ONLY a Forkable
	// resource; a Convergent/Singular (or silent→singular) one is DEFERRED, never
	// auto-forked. The routed dir and env name are derived COLLISION-FREE from a
	// content hash of the (caller-controlled) ref, and a resource name may never
	// overwrite a substrate-owned env key (buildEnv ran first) or a prior resource.
	deferred := make([]DeferredResource, 0, len(req.Resources))
	for _, r := range req.Resources {
		if strings.TrimSpace(r.Ref) == "" {
			return nil, fmt.Errorf("substrate: resource ref required")
		}
		k := s.cls.Classify(resourceRef(r))
		if !shouldIsolate(k) {
			deferred = append(deferred, DeferredResource{Name: r.Ref, Kind: k.String()})
			continue
		}
		resSlug := resourceSlug(r.Ref) // collision-free, never empty
		var p string
		p, err = Contain(agentRoot, filepath.Join("res", resSlug))
		if err != nil {
			return nil, fmt.Errorf("substrate: route resource %q: %w", r.Ref, err)
		}
		if err = os.MkdirAll(p, 0o700); err != nil {
			return nil, fmt.Errorf("substrate: route resource %q: %w", r.Ref, err)
		}
		name := r.Name
		if name == "" {
			name = resourceEnvName(resSlug)
		}
		if err = validateResourceEnvName(name, env); err != nil {
			return nil, fmt.Errorf("substrate: route resource %q: %w", r.Ref, err)
		}
		env[name] = p
	}
	sort.Slice(deferred, func(i, j int) bool { return deferred[i].Name < deferred[j].Name })

	spec = &EnvSpec{
		session:      session,
		grain:        s.grain.Name(),
		cwd:          boundary.Cwd,
		branch:       boundary.Branch,
		hostWorktree: boundary.HostWorktree,
		containerID:  boundary.ContainerID,
		ports:        ports,
		env:          env,
		stateDirs:    stateDirs,
		stateRoot:    agentRoot,
		deferred:     deferred,
	}

	s.mu.Lock()
	delete(s.reserving, slug) // the reservation becomes a completed live boundary
	s.live[slug] = spec
	s.mu.Unlock()
	built = true
	return spec, nil
}

// buildEnv assembles the routed runtime env: per-agent local-state paths and the
// per-agent port block. Classifier-gated resource paths are added by Provision.
func (s *Substrate) buildEnv(session string, ports []int, stateDirs map[string]string) map[string]string {
	env := map[string]string{
		"MAD_SESSION": session,
		"MAD_SCRATCH": stateDirs["scratch"],
		"MAD_CACHE":   stateDirs["cache"],
		"MAD_STATE":   stateDirs["state"],
		// Route the common toolchain state vars at the per-agent dirs so a tool
		// that honors them writes into THIS agent's private state, not a shared one.
		"TMPDIR":         stateDirs["scratch"],
		"XDG_CACHE_HOME": stateDirs["cache"],
		"XDG_STATE_HOME": stateDirs["state"],
	}
	if len(ports) > 0 {
		strs := make([]string, len(ports))
		for i, p := range ports {
			strs[i] = strconv.Itoa(p)
			env["MAD_PORT_"+strconv.Itoa(i)] = strconv.Itoa(p)
		}
		env["PORT"] = strconv.Itoa(ports[0]) // primary
		env["MAD_PORTS"] = strings.Join(strs, ",")
	}
	return env
}

// Teardown removes the boundary for session and frees its ports/state. It is the
// MECHANISM the launcher invokes on clean exit and liveness on crash. Idempotent:
// a no-op (nil) if the session has no live boundary.
func (s *Substrate) Teardown(session string) error {
	slug := safeSlug(session)
	s.mu.Lock()
	spec, ok := s.live[slug]
	if !ok {
		// If a lifecycle op (a provision, or another teardown) is in flight for this
		// slug it owns the slug-derived paths — leave them alone.
		if _, busy := s.reserving[slug]; busy {
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		// Nothing live AND nothing in flight. This is the daemon-RESTART case: a
		// fresh substrate has an empty live map, so liveness's restart-reattachment
		// (reconstructing a dead holder from the durable lease ledger) reaches HERE
		// for a session whose container/worktree may still be RUNNING on the host.
		// Reclaim the orphan by its DETERMINISTIC name/path so the boundary is freed
		// instead of leaked forever. Best-effort: log, never error the caller (the
		// lease reclamation must still proceed even if the runtime is unavailable).
		if err := s.grain.ReclaimOrphan(slug); err != nil {
			log.Printf("substrate: reclaim orphan for session %q (live-map miss): %v", session, err)
		}
		// Also reclaim the reconstructed per-agent local-state root (same derivation
		// as provisionState: stateBase(repo)/slug) — it too would leak across a restart.
		if err := removeState(filepath.Join(stateBase(s.repoAbs), slug)); err != nil {
			log.Printf("substrate: reclaim orphan state for session %q: %v", session, err)
		}
		return nil
	}
	// Move the slug from live into the in-flight set and HOLD it there across the
	// destructive step (which runs outside the lock). This blocks a concurrent
	// re-Provision of the same slug from building a NEW boundary on the same paths
	// while this teardown is still deleting them — the Teardown-vs-Provision analog
	// of the reserve-first guard (without it, a stale teardown nukes a live
	// re-provisioned boundary at an idempotent grain).
	delete(s.live, slug)
	s.reserving[slug] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.reserving, slug)
		s.mu.Unlock()
	}()

	s.ports.release(spec.ports)
	var firstErr error
	if err := removeState(spec.stateRoot); err != nil {
		firstErr = err
	}
	if err := s.grain.Teardown(Boundary{
		Cwd:          spec.cwd,
		Branch:       spec.branch,
		HostWorktree: spec.hostWorktree,
		ContainerID:  spec.containerID,
	}); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Lookup returns the live EnvSpec for a session (read-only; for diagnostics /
// the launcher to re-fetch). Nil if none.
func (s *Substrate) Lookup(session string) *EnvSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[safeSlug(session)]
}

func resourceRef(r ResourceReq) manifest.ResourceRef {
	switch r.Domain {
	case "path":
		return manifest.PathRef(r.Ref)
	case "external":
		return manifest.ExternalRef(r.Ref)
	default:
		// Unknown domain → classify-upward → singular (never auto-forked).
		return manifest.ResourceRef{Domain: manifest.Domain(-1), Name: r.Ref}
	}
}

// safeSlug maps an arbitrary session id to a filesystem- and git-ref-safe,
// collision-resistant slug. Daemon session ids ("s-3-<hex>") are already safe, so
// the common case is identity; a disambiguating hash is appended ONLY if
// sanitization changed the string (so distinct unsafe inputs can't collide). The
// reserve-first discipline keys the live map on THIS slug, so even a hypothetical
// collision collapses to one boundary rather than racing.
func safeSlug(s string) string {
	out := strings.Trim(sanitize(s), "-_")
	if out == "" {
		return ""
	}
	if out != s {
		h := sha256.Sum256([]byte(s))
		out = out + "-" + hex.EncodeToString(h[:4])
	}
	return out
}

// resourceSlug derives a collision-free, never-empty path segment for a routed
// resource from its (caller-controlled) ref: a sanitized prefix for readability
// plus an UNCONDITIONAL content hash, so two distinct refs can never share a
// routed dir, and an all-unsafe or empty-after-sanitize ref still yields a valid,
// unique slug.
func resourceSlug(ref string) string {
	san := strings.Trim(sanitize(ref), "-_")
	if san == "" {
		san = "res"
	}
	h := sha256.Sum256([]byte(ref))
	return san + "-" + hex.EncodeToString(h[:6])
}

// resourceEnvName derives the auto env var name for a routed forkable resource: a
// valid env identifier (dashes → underscores), unique (the slug carries a content
// hash), in the MAD_RES_ namespace.
func resourceEnvName(resSlug string) string {
	return "MAD_RES_" + strings.ToUpper(strings.ReplaceAll(resSlug, "-", "_"))
}

// validateResourceEnvName rejects a routed-resource env name that is not a valid
// env identifier, that would shadow a launch-critical variable (PATH, LD_*, …),
// or that collides with a substrate-owned key / a prior resource. The substrate
// EMITS the env-spec the launcher applies (Inv 1), so it must never emit a name
// that could redirect the launched process's binary/library/shell resolution or
// (via an embedded '=') decompose into a different variable.
func validateResourceEnvName(name string, env map[string]string) error {
	if !isEnvIdent(name) {
		return fmt.Errorf("env name %q is not a valid identifier", name)
	}
	if isLaunchSensitiveEnvName(name) {
		return fmt.Errorf("env name %q is reserved (launch-critical)", name)
	}
	if _, taken := env[name]; taken {
		return fmt.Errorf("env name %q is reserved or already assigned", name)
	}
	return nil
}

// isEnvIdent reports whether s is a POSIX-shape env identifier
// [A-Za-z_][A-Za-z0-9_]* — in particular it contains no '=', which would split
// into a different variable when the launcher applies the env.
func isEnvIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// isLaunchSensitiveEnvName reports whether name controls how the launched process
// resolves binaries/libraries or interprets the shell — a routed resource must
// never be allowed to shadow these (defense beyond the substrate-owned env
// membership check; the env-spec must be safe by construction).
func isLaunchSensitiveEnvName(name string) bool {
	switch name {
	case "PATH", "HOME", "SHELL", "IFS", "ENV", "BASH_ENV", "PWD", "OLDPWD", "USER", "LOGNAME":
		return true
	}
	for _, p := range []string{"LD_", "DYLD_", "GIT_", "SSH_"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// sanitize maps an arbitrary string to [A-Za-z0-9_-], replacing every other rune
// with '_'.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
