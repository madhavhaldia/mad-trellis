// Package app is the composition root: it wires the daemon (project 1) to host
// the lease ledger (project 2), the classifier (project 3), the isolation
// substrate (project 4), and the trunk integrator (project 6), installs the
// lease's durable audit sink behind the daemon's audit interface, registers the
// frozen lease.* / classify.* / substrate.* / integrate.* JSON-RPC methods, and
// freezes the registry before serving. This is the integration pattern the waves
// extend: an in-daemon component constructs its Go core and registers its RPC
// methods here, before the freeze.
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/daemon"
	"github.com/madhavhaldia/mad-substrate/internal/integration"
	"github.com/madhavhaldia/mad-substrate/internal/integrator"
	"github.com/madhavhaldia/mad-substrate/internal/lease"
	"github.com/madhavhaldia/mad-substrate/internal/liveness"
	"github.com/madhavhaldia/mad-substrate/internal/manifest"
	"github.com/madhavhaldia/mad-substrate/internal/session"
	"github.com/madhavhaldia/mad-substrate/internal/singular"
	"github.com/madhavhaldia/mad-substrate/internal/substrate"
)

// Config configures the composed daemon.
type Config struct {
	SocketPath string // Unix socket the daemon binds
	LedgerPath string // SQLite ledger file ("" / ":memory:" for in-memory)
	RepoRoot   string // where the manifest is loaded from

	// TrunkDir / TrunkBranch / IntegratorStorePath configure project 6's mediated
	// trunk. All optional: empty values derive from LedgerPath's directory so the
	// daemon and tests need not spell them out.
	TrunkDir            string
	TrunkBranch         string
	IntegratorStorePath string
}

// Build composes the daemon hosting the lease ledger, classifier, substrate, and
// integrator. The returned close func releases all durable resources; call it
// after the daemon stops. The daemon is constructed and frozen but NOT started —
// the caller calls Start then Serve.
func Build(cfg Config) (d *daemon.Daemon, closeAll func() error, err error) {
	// YANKED / CORRUPT / LOCKED LEDGER (chafe C14): a daemon that cannot open its
	// durable ledger MUST refuse to serve with a CLEAR, actionable error — never a
	// raw SQLite "disk I/O error (522)" leaking from the driver. A fresh Open
	// recovers stale "-wal"/"-shm" sidecars from an unclean shutdown on its own; if
	// it still can't, this is where the operator sees why. We do NOT delete a
	// ledger we did not create — we only surface the reason.
	l, err := lease.Open(cfg.LedgerPath, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger at %s is unusable: %v; refusing to serve", ledgerLabel(cfg.LedgerPath), err)
	}
	m, err := manifest.Load(cfg.RepoRoot)
	if err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	cls := manifest.New(m)

	// The daemon's audit interface is backed by the lease ledger's durable,
	// append-only sink (edge lease->daemon). The SAME sink also implements the
	// read-only AuditReader (Tail), powering the watch-view surface's audit.tail
	// without giving any other path a second store edge — storage stays the lease
	// ledger's job, behind the daemon's method.
	sink := l.AuditSink()
	d = daemon.New(daemon.Options{SocketPath: cfg.SocketPath, Audit: sink, AuditReader: sink})

	if err := lease.RegisterMethods(d.Registry(), l); err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	if err := manifest.RegisterMethods(d.Registry(), cls); err != nil {
		_ = l.Close()
		return nil, nil, err
	}

	// P2: the forkable isolation substrate, gated by the classifier verdict
	// (Inv 1 + 10-grainswap). It constructs per-agent worktree/ports/local-state
	// boundaries and returns an env-spec; the launcher (P3) applies it.
	sub, err := substrate.New(cfg.RepoRoot, cls, substrate.Options{})
	if err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	if err := substrate.RegisterMethods(d.Registry(), sub); err != nil {
		_ = l.Close()
		return nil, nil, err
	}

	// P6: the trunk integrator — the SOLE promoter of the canonical trunk. It owns
	// a mediated holding/trunk repo (agents push there; only the integrator
	// advances trunk, via a lease-gated atomic update-ref). Single-writer is the
	// LEASE: the integrator consumes the ledger's CAS through a thin adapter and
	// never imports the lease store. Decision-audit is emitted through the same
	// durable sink (edge integrator->audit interface).
	trunkDir, trunkBranch, storePath := integratorPaths(cfg)
	if _, err := integrator.EnsureMediatedRepo(trunkDir, trunkBranch); err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	itg, err := integrator.New(integrator.Options{
		TrunkDir:    trunkDir,
		TrunkBranch: trunkBranch,
		StorePath:   storePath,
		Leases:      leaseGate{l},
		TrunkKey:    manifest.TrunkKey,
		Audit: func(session, kind string, payload []byte) {
			_ = sink.Append(daemon.AuditRecord{
				Timestamp:       time.Now(),
				Session:         daemon.SessionID(session),
				DecisionProject: "integrator-trunk",
				DecisionKind:    kind,
				Payload:         payload,
			})
		},
	})
	if err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	if err := integrator.RegisterMethods(d.Registry(), itg); err != nil {
		_ = itg.Close()
		_ = l.Close()
		return nil, nil, err
	}

	// P7: the singular-gate — the default-deny boundary for resources with real
	// external side effects (Inv 8). It CONSUMES the classifier's verdict (never
	// reimplements classification), serializes supervised grants on the LEDGER's
	// CAS over an opaque singular key (never its own lock), and produces the
	// singular portion of the env-spec (deny/mock/proxy never carry the real
	// endpoint). DENY is the structural ground state.
	gate, err := singular.New(singular.Options{
		Classifier: cls,
		Grants:     m.Grants,
		Leases:     leaseGate{l},
		Audit: func(session, kind string, payload []byte) {
			_ = sink.Append(daemon.AuditRecord{
				Timestamp:       time.Now(),
				Session:         daemon.SessionID(session),
				DecisionProject: "singular-gate",
				DecisionKind:    kind,
				Payload:         payload,
			})
		},
	})
	if err != nil {
		_ = itg.Close()
		_ = l.Close()
		return nil, nil, err
	}
	if err := singular.RegisterMethods(d.Registry(), gate); err != nil {
		_ = itg.Close()
		_ = l.Close()
		return nil, nil, err
	}

	// Wing 3: the integration plane — the daemon-side review/verdict queue an
	// external integrator agent drives (a builder REQUESTS integration of its
	// boundary branch; the integrator CLAIMS + records a VERDICT; the builder polls
	// STATUS). This is PURE records + a state machine: no git, no merge (the merge
	// is the integrator-trunk plane above). Its single-writer ledger is a sibling to
	// the integrator's under the same runtime dir. Identity is the connection-bound
	// session (Inv 4) on every mutation. It is constructed BEFORE the Recoverer so
	// the liveness sweep can revert a review request stranded in `claimed` by a dead
	// integrator (a crashed claimer no longer strands a request forever).
	ign, err := integration.New(integration.Options{
		StorePath: integrationStorePath(cfg),
		HoldsIntegratorPresence: func(session string) bool {
			return holdsIntegratorPresence(l, session)
		},
		Audit: func(session, kind string, payload []byte) {
			_ = sink.Append(daemon.AuditRecord{
				Timestamp:       time.Now(),
				Session:         daemon.SessionID(session),
				DecisionProject: "integration-review",
				DecisionKind:    kind,
				Payload:         payload,
			})
		},
	})
	if err != nil {
		_ = itg.Close()
		_ = l.Close()
		return nil, nil, err
	}
	if err := integration.Register(d.Registry(), ign); err != nil {
		_ = ign.Close()
		_ = itg.Close()
		_ = l.Close()
		return nil, nil, err
	}

	// P8: liveness-recovery — the crash-path detector+trigger. It reads the
	// ledger's EXPIRED leases (the death signal) and invokes ReclaimIfExpired (the
	// CAS lives in the ledger), aborts a dead mid-integration holder's integration
	// (the integrator's idempotent Abort — one-way dep), tears down its boundary
	// (the substrate's Teardown), and reverts a review request stranded in `claimed`
	// by a dead integrator back to `requested` (the integration plane's
	// ReclaimStaleClaims, gated by the SAME live-set death oracle). It NEVER mutates
	// the ledger/trunk directly. A background loop scans periodically; the first scan
	// on start IS the restart-reattachment pass (a holder that died while the daemon
	// was down left an expired lease in the durable ledger).
	rec, err := liveness.NewWithIntegrationReclaim(
		leaseReclaimer{l},
		integAborter{itg},
		sub,                         // *substrate.Substrate satisfies BoundaryReclaimer (Teardown(session) error)
		manifest.TrunkKey,           // the convergent key whose expiry signals a mid-promote death
		session.LivenessKeyPrefix(), // T2: an expired session-liveness lease is the canonical session-death signal
		integClaimReclaimer{ign},    // Wing 3: revert claims stranded by a dead integrator
		func(session, kind string, payload []byte) {
			_ = sink.Append(daemon.AuditRecord{
				Timestamp:       time.Now(),
				Session:         daemon.SessionID(session),
				DecisionProject: "liveness-recovery",
				DecisionKind:    kind,
				Payload:         payload,
			})
		},
	)
	if err != nil {
		_ = ign.Close()
		_ = itg.Close()
		_ = l.Close()
		return nil, nil, err
	}
	if err := liveness.RegisterMethods(d.Registry(), rec); err != nil {
		_ = ign.Close()
		_ = itg.Close()
		_ = l.Close()
		return nil, nil, err
	}

	// T2: the session-identity/liveness UNIFIER. The capability-token store lets all
	// of one agent's processes share ONE daemon session identity: session.mint_token
	// mints an unforgeable token bound to the holder's connection; session.attach
	// rebinds a new connection to that identity iff the session-liveness lease is
	// still held (the ONE TRUE death signal). The store takes a READ-ONLY lease seam
	// (sessionLeaseReader) plus a DURABLE token seam (sessionTokenStore) — both
	// interfaces over the ledger so the session package never imports it. The token
	// binding is DURABLE (P0 #4) so session.attach survives a daemon restart: the
	// launcher re-attaches via its token instead of the restart orphaning a
	// still-live session whose boundary liveness would then reclaim.
	sessTokens := session.NewStore(sessionLeaseReader{l}, sessionTokenStore{l})
	if err := session.RegisterMethods(d.Registry(), sessTokens); err != nil {
		_ = ign.Close()
		_ = itg.Close()
		_ = l.Close()
		return nil, nil, err
	}

	// Freeze the contract surface before serving: after this, the method set is
	// immutable (changing it requires re-review). Downstream waves register their
	// methods here, before Build returns, not after the freeze.
	d.Registry().Freeze()

	// Start the periodic recovery loop; closeAll cancels it.
	ctx, cancel := context.WithCancel(context.Background())
	go rec.Loop(ctx, 5*time.Second)

	return d, func() error {
		cancel()
		_ = ign.Close()
		_ = itg.Close()
		return l.Close()
	}, nil
}

// leaseReclaimer adapts the lease ledger to liveness.LeaseReclaimer (read the
// expired leases; invoke the reclaim CAS — liveness never writes the table).
type leaseReclaimer struct{ l *lease.Ledger }

func (a leaseReclaimer) ExpiredLeases() ([]liveness.ExpiredLease, error) {
	infos, err := a.l.ExpiredLeases()
	if err != nil {
		return nil, err
	}
	out := make([]liveness.ExpiredLease, 0, len(infos))
	for _, i := range infos {
		out = append(out, liveness.ExpiredLease{Key: i.Key, Holder: i.Holder})
	}
	return out, nil
}

func (a leaseReclaimer) LiveHolders() ([]string, error) {
	infos, err := a.l.ListHolders()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Holder)
	}
	return out, nil
}

func (a leaseReclaimer) ReclaimIfExpired(key []byte) (bool, string, error) {
	res, err := a.l.ReclaimIfExpired(key)
	if err != nil {
		return false, "", err
	}
	return res.Reclaimed, res.PriorHolder, nil
}

// integAborter adapts the integrator to liveness.IntegrationAborter (one-way:
// liveness invokes the integrator's idempotent Abort; the integrator never
// imports liveness).
type integAborter struct{ it *integrator.Integrator }

func (a integAborter) InFlight() ([]liveness.InFlightIntegration, error) {
	recs, err := a.it.InFlight()
	if err != nil {
		return nil, err
	}
	out := make([]liveness.InFlightIntegration, 0, len(recs))
	for _, r := range recs {
		out = append(out, liveness.InFlightIntegration{ID: r.ID, Holder: r.Holder, State: string(r.State)})
	}
	return out, nil
}

func (a integAborter) Abort(id string) error {
	_, err := a.it.Abort(id)
	return err
}

// integClaimReclaimer adapts the integration plane to
// liveness.IntegrationClaimReclaimer (one-way: liveness supplies the death oracle
// via isDead; the integration plane owns the claimed->requested CAS and never
// imports liveness). The sweep reverts a review request stranded in `claimed` by
// a dead integrator so a crashed claimer never strands it forever.
type integClaimReclaimer struct{ ig *integration.Integration }

func (a integClaimReclaimer) ReclaimStaleClaims(isDead func(session string) bool) (int, error) {
	return a.ig.ReclaimStaleClaims(isDead)
}

// GCStale garbage-collects aged-out review records (terminal verdicts + long-
// abandoned requests) by their own TTL each liveness scan, so the record store does
// not grow without bound. Not death-gated and never touches a `claimed` row.
func (a integClaimReclaimer) GCStale() (int, error) { return a.ig.GCStale() }

const integratorPresenceKey = "mad-substrate:integrator:v1"

func holdsIntegratorPresence(l *lease.Ledger, session string) bool {
	holders, err := l.ListHolders()
	if err != nil {
		return false
	}
	for _, h := range holders {
		if h.Holder != session {
			continue
		}
		key := string(h.Key)
		if key == integratorPresenceKey || strings.HasPrefix(key, integratorPresenceKey+":slot-") {
			return true
		}
	}
	return false
}

// ledgerLabel renders a ledger path for the unusable-ledger error message. An
// empty/":memory:" path (the in-memory test/dev ledger) is named explicitly so
// the message reads sensibly rather than printing an empty path.
func ledgerLabel(path string) string {
	if path == "" || path == ":memory:" {
		return ":memory:"
	}
	return path
}

// integratorPaths resolves project 6's trunk dir / branch / store path, deriving
// any unset value from the ledger's directory (so a real-ledger daemon and the
// in-memory-ledger tests both get a self-consistent on-disk mediated trunk).
func integratorPaths(cfg Config) (trunkDir, trunkBranch, storePath string) {
	trunkBranch = cfg.TrunkBranch
	if trunkBranch == "" {
		trunkBranch = "trunk"
	}
	dir := filepath.Dir(cfg.LedgerPath)
	if cfg.LedgerPath == "" || cfg.LedgerPath == ":memory:" {
		dir = "." // only reached by in-memory-ledger callers; the daemon passes real paths
	}
	trunkDir = cfg.TrunkDir
	if trunkDir == "" {
		trunkDir = filepath.Join(dir, "trunk.git")
	}
	storePath = cfg.IntegratorStorePath
	if storePath == "" {
		storePath = filepath.Join(dir, "integrator.db")
	}
	return trunkDir, trunkBranch, storePath
}

// integrationStorePath resolves Wing 3's request-ledger path, a sibling to the
// integrator's under the same runtime dir (derived from the lease ledger's
// directory, mirroring integratorPaths). An in-memory ledger (tests/dev) gets an
// in-memory request store so it leaves nothing on disk.
func integrationStorePath(cfg Config) string {
	if cfg.LedgerPath == "" || cfg.LedgerPath == ":memory:" {
		return ":memory:"
	}
	return filepath.Join(filepath.Dir(cfg.LedgerPath), "integration.db")
}

// sessionLeaseReader adapts the lease ledger to session.SessionLeaseChecker: a
// READ-ONLY single-key live-holder read (Inspect only — no CAS, no mutation). It
// is the seam session.attach uses to confirm a session-liveness lease is still
// held; the session store never imports or writes the ledger.
type sessionLeaseReader struct{ l *lease.Ledger }

func (r sessionLeaseReader) LiveHolder(key []byte) (string, bool, error) {
	info, _, err := r.l.Inspect(key)
	if err != nil {
		return "", false, err
	}
	return info.Holder, info.Held, nil
}

// sessionTokenStore adapts the lease ledger to session.TokenStore: the DURABLE
// token-hash -> sessionID binding that lets session.attach survive a daemon
// restart (P0 #4). Only the token hash is persisted (never the raw bearer secret).
// The session store reaches the ledger ONLY through this interface.
type sessionTokenStore struct{ l *lease.Ledger }

func (s sessionTokenStore) Put(tokenHash, sessionID string, createdMs int64) error {
	return s.l.PutSessionToken(tokenHash, sessionID, createdMs)
}

func (s sessionTokenStore) Get(tokenHash string) (string, bool, error) {
	return s.l.GetSessionToken(tokenHash)
}

func (s sessionTokenStore) Delete(tokenHash string) error {
	return s.l.DeleteSessionToken(tokenHash)
}

func (s sessionTokenStore) List() ([]session.TokenBinding, error) {
	rows, err := s.l.ListSessionTokens()
	if err != nil {
		return nil, err
	}
	out := make([]session.TokenBinding, 0, len(rows))
	for _, r := range rows {
		out = append(out, session.TokenBinding{TokenHash: r.TokenHash, SessionID: r.SessionID, CreatedMs: r.CreatedMs})
	}
	return out, nil
}

// leaseGate adapts the lease ledger to the integrator's LeaseGate (single-writer
// is the LEASE, not a private mutex — the integrator consumes this and never
// imports the lease store).
type leaseGate struct{ l *lease.Ledger }

func (g leaseGate) Acquire(key []byte, holder string, ttl time.Duration) (bool, string, error) {
	res, err := g.l.Acquire(key, holder, ttl)
	if err != nil {
		return false, "", err
	}
	return res.Granted, res.Holder, nil
}

// Renew extends a held lease (the singular gate's supervised-grant heartbeat;
// the integrator's LeaseGate does not use it, but a struct may satisfy both).
func (g leaseGate) Renew(key []byte, holder string, ttl time.Duration) (bool, error) {
	res, err := g.l.Renew(key, holder, ttl)
	if err != nil {
		return false, err
	}
	return res.OK, nil
}

func (g leaseGate) Release(key []byte, holder string) (bool, error) {
	return g.l.Release(key, holder)
}
