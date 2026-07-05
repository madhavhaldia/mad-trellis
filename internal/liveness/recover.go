// Package liveness implements project 8 (liveness-recovery): the crash path only.
// "No lock outlives its holder; the system makes progress after any single death"
// — as a strict DETECTOR + TRIGGER, never a mutator.
//
// Owns (docs/0003 clause map): 3-reclaim — dead-holder DETECTION + TRIGGER only.
// It NEVER mutates the ledger or trunk directly: it INVOKES the ledger's
// ReclaimIfExpired (the CAS lives there) and the integrator's idempotent
// Abort(id) (the abort lives there) and the substrate's Teardown. It defines no
// locking semantics, runs no app-level retries, and — the no-dispatch line —
// NEVER re-launches or resumes the dead agent's work: recovery FREES resources,
// it does not resume them (Inv 13).
//
// CARDINAL HAZARD — FALSE-POSITIVE DEATH: reclaiming/destroying a slow-but-LIVE
// holder's state hands a key to a second writer or wipes a live sandbox (Inv 9
// downward = corruption). The guarantees, in layers:
//   - Lease RECLAIM is per-KEY and idempotent: the detector reads only EXPIRED
//     leases (now >= expires_at; a renewing holder is never expired), and
//     ReclaimIfExpired's own CAS frees ONLY a still-expired lease.
//   - A HOLDER is never declared dead from a lapsed UNRELATED key. A daemon
//     session holds MANY independent leases (a transient trunk lease per promote,
//     per-resource singular grants), each with its own TTL. So: an integration is
//     aborted only when its holder holds NO live lease (its promoter is gone), and
//     a boundary is torn down only for a holder whose TRUNK lease expired
//     (a confirmed mid-promote death) AND who holds no live lease. A holder still
//     holding ANY live lease is ALIVE and is never touched.
//
// One-way dependency: liveness depends on the integrator (it invokes Abort), the
// integrator NEVER depends on liveness.
package liveness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
)

// ExpiredLease is a held lease past its TTL — the per-key death signal.
type ExpiredLease struct {
	Key    []byte
	Holder string
}

// LeaseReclaimer is the ledger surface liveness consumes: READ the expired leases
// (the signal) + the live holders (the liveness cross-check), and INVOKE the
// reclaim CAS (the trigger). Liveness never writes the lease table itself.
type LeaseReclaimer interface {
	ExpiredLeases() ([]ExpiredLease, error)
	LiveHolders() ([]string, error)
	ReclaimIfExpired(key []byte) (reclaimed bool, priorHolder string, err error)
}

// InFlightIntegration is a non-terminal integration (received/validating).
type InFlightIntegration struct {
	ID     string
	Holder string
	State  string // "received" | "validating"
}

// IntegrationAborter is the integrator surface liveness consumes (one-way): list
// in-flight integrations and INVOKE the idempotent abort. Abort reconciles
// against git, so aborting an integration whose promote actually landed is a safe
// no-op (it reports promoted), and trunk is never left mid-integration.
type IntegrationAborter interface {
	InFlight() ([]InFlightIntegration, error)
	Abort(id string) error
}

// BoundaryReclaimer is the substrate surface liveness consumes: tear down a dead
// holder's boundary (idempotent — a no-op if there is none).
type BoundaryReclaimer interface {
	Teardown(session string) error
}

// IntegrationClaimReclaimer is the integration-plane surface liveness consumes
// (one-way): revert every review request stranded in `claimed` by a DEAD
// integrator back to `requested`. isDead is liveness's authority (a session that
// holds no live lease). It mirrors the lease-reclaim shape — liveness supplies
// the death oracle, the integration plane owns the CAS. The integration plane
// never imports liveness.
type IntegrationClaimReclaimer interface {
	ReclaimStaleClaims(isDead func(session string) bool) (int, error)
	// GCStale garbage-collects aged-out review records (terminal verdicts and
	// long-abandoned requests) by their own TTL, returning the count deleted. It is
	// NOT death-gated — coldness, not holder liveness, is the trigger — so it never
	// reclaims a `claimed` (in-flight) row. Called best-effort each scan.
	GCStale() (int, error)
}

// AuditFunc emits a recovery-audit record (nil → no-op).
type AuditFunc func(session, kind string, payload []byte)

// Recoverer is the detector+trigger. integ and bound are OPTIONAL (nil → that
// recovery step is skipped), keeping the package unit-testable in isolation.
type Recoverer struct {
	leases     LeaseReclaimer
	integ      IntegrationAborter
	bound      BoundaryReclaimer
	intReclaim IntegrationClaimReclaimer // OPTIONAL (nil → the stale-claim sweep is skipped)
	audit      AuditFunc
	trunkKey   []byte // the convergent key whose expiry signals a mid-promote death
	sessionKey []byte // the session-liveness key PREFIX ("mad-substrate:session:v1:") whose
	// expiry signals a SESSION death (the launcher stopped renewing): a session whose
	// session-liveness lease expired AND that holds no other live lease is dead, so its
	// boundary is reclaimed (T2 — the canonical session-death signal, closing C16/C17).
}

// New constructs a Recoverer. leases is required; trunkKey (the convergent lease
// key) identifies a mid-promote death for boundary recovery.
//
// DEPRECATED-shape note: prefer NewWithSessionKey to ALSO key boundary recovery
// off the session-liveness lease (T2). New keeps the pre-T2 trunk-only behavior
// for callers/tests that do not wire the session-liveness signal.
func New(leases LeaseReclaimer, integ IntegrationAborter, bound BoundaryReclaimer, trunkKey []byte, audit AuditFunc) (*Recoverer, error) {
	return NewWithSessionKey(leases, integ, bound, trunkKey, nil, audit)
}

// NewWithSessionKey constructs a Recoverer that ALSO treats an expired
// session-liveness lease (whose key begins with sessionKeyPrefix) as a session
// death for boundary recovery (T2). sessionKeyPrefix is the raw
// "mad-substrate:session:v1:" prefix; an expired lease under it whose holder holds NO
// other live lease (Inv 17) has its boundary torn down. nil/empty prefix → the
// session-death signal is disabled (the pre-T2 trunk-only behavior).
func NewWithSessionKey(leases LeaseReclaimer, integ IntegrationAborter, bound BoundaryReclaimer, trunkKey, sessionKeyPrefix []byte, audit AuditFunc) (*Recoverer, error) {
	return NewWithIntegrationReclaim(leases, integ, bound, trunkKey, sessionKeyPrefix, nil, audit)
}

// NewWithIntegrationReclaim is NewWithSessionKey plus the OPTIONAL integration
// stale-claim sweep: each scan reverts a review request stranded in `claimed` by
// a DEAD integrator (a session holding no live lease) back to `requested`, so a
// crashed integrator never strands a request forever. A nil integReclaim disables
// the sweep (the pre-Wing-3 behavior). The death oracle is the same `live` set the
// boundary/abort steps use — a claimer holding ANY live lease is never reclaimed.
func NewWithIntegrationReclaim(leases LeaseReclaimer, integ IntegrationAborter, bound BoundaryReclaimer, trunkKey, sessionKeyPrefix []byte, integReclaim IntegrationClaimReclaimer, audit AuditFunc) (*Recoverer, error) {
	if leases == nil {
		return nil, fmt.Errorf("liveness: a LeaseReclaimer is required")
	}
	if audit == nil {
		audit = func(string, string, []byte) {}
	}
	return &Recoverer{leases: leases, integ: integ, bound: bound, intReclaim: integReclaim, trunkKey: trunkKey, sessionKey: sessionKeyPrefix, audit: audit}, nil
}

// Report summarizes one Scan (diagnostics; the recovery is the side effect).
type Report struct {
	Reclaimed       int
	Aborted         int
	TornDown        int
	ReclaimedClaims int // review requests reverted from `claimed` (dead integrator) -> `requested`
	GCedRecords     int // aged-out review records (terminal verdicts / abandoned requests) deleted by TTL
	DeadHolders     []string
}

// Scan performs ONE recovery pass. It is IDEMPOTENT and BEST-EFFORT: every step
// is attempted independently and errors are accumulated (not short-circuited), so
// one failure never strands the rest of the pass, and a later scan re-attempts.
// It NEVER touches a holder that still holds a live lease.
func (r *Recoverer) Scan() (Report, error) {
	rep := Report{}
	var errs error

	expired, err := r.leases.ExpiredLeases()
	if err != nil {
		return rep, err // cannot read the signal → do nothing (no harm)
	}

	// 1) Reclaim every expired lease (per-key, always correct + idempotent). Record
	//    which holders had their TRUNK lease reclaimed — a confirmed mid-promote death —
	//    and which had their SESSION-LIVENESS lease reclaimed (the launcher stopped
	//    renewing → a confirmed session death, T2). Both are death SIGNALS; the live
	//    cross-check below (Inv 17) decides whether the holder is actually dead.
	trunkDead := map[string]bool{}
	sessionDead := map[string]bool{}
	for _, e := range expired {
		reclaimed, prior, err := r.leases.ReclaimIfExpired(e.Key)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		if reclaimed {
			rep.Reclaimed++
			r.audit(prior, "liveness.reclaimed", jpayload(`{"holder":%q,"key":%q}`, prior, string(e.Key)))
			if prior != "" && len(r.trunkKey) > 0 && bytes.Equal(e.Key, r.trunkKey) {
				trunkDead[prior] = true
			}
			if prior != "" && len(r.sessionKey) > 0 && bytes.HasPrefix(e.Key, r.sessionKey) {
				sessionDead[prior] = true
			}
		}
	}

	// 2) Determine which holders are still ALIVE (hold ANY live lease). A live
	//    holder is NEVER declared dead, so a lapsed unrelated key (e.g. a singular
	//    grant) can never cause its integration/boundary to be touched.
	live, lerr := r.liveSet()
	errs = errors.Join(errs, lerr)

	// 3) ABORT a stuck mid-promote integration: one that is `validating` (its
	//    promoter wrote the merge commit) whose holder holds NO live lease → the
	//    promoter is gone. Idempotent + reconciles against git, so it is safe and
	//    eventually-complete (re-attempted each scan from durable InFlight). A
	//    `received` integration holds no lock and endangers no trunk, so it is left.
	if r.integ != nil {
		inflight, err := r.integ.InFlight()
		if err != nil {
			errs = errors.Join(errs, err)
		} else {
			for _, in := range inflight {
				if in.State == "validating" && !live[in.Holder] {
					if err := r.integ.Abort(in.ID); err != nil {
						errs = errors.Join(errs, err)
						continue
					}
					rep.Aborted++
					r.audit(in.Holder, "liveness.aborted", jpayload(`{"id":%q}`, in.ID))
				}
			}
		}
	}

	// The set of holders whose death SIGNAL fired: a mid-promote death (TRUNK lease
	// expired) OR a session death (SESSION-LIVENESS lease expired — the launcher
	// stopped renewing, T2). Either is a candidate for boundary teardown; the live
	// cross-check (Inv 17) below is the authority on whether the holder is actually
	// dead.
	deathSignal := map[string]bool{}
	for h := range trunkDead {
		deathSignal[h] = true
	}
	for h := range sessionDead {
		deathSignal[h] = true
	}

	// 4) TEAR DOWN the boundary of a CONFIRMED death — a holder whose trunk lease
	//    expired (mid-promote death) OR whose session-liveness lease expired (session
	//    death, T2) — that holds NO live lease. CONSERVATIVE by design (Inv 17): a
	//    lapsed UNRELATED lease NEVER triggers teardown, and a holder still holding
	//    ANY live lease (including a still-renewed session-liveness lease) is skipped —
	//    so a live agent's sandbox is never destroyed from one lapsed lease.
	//
	//    THE HARD-KILL BACKSTOP: this is the daemon-side reap that covers the path the
	//    launcher's clean-exit teardown CANNOT (a launcher SIGKILL / power loss, where
	//    no graceful drop ran). The session-liveness lease lapses → the session is
	//    declared dead → its worktree/boundary + any leases under the shared id are
	//    reclaimed here, independent of HOW the session died.
	//
	//    SCOPE — best-effort, and worktree/lease only: r.bound.Teardown reclaims the
	//    daemon-owned resources (the boundary worktree, its port reservation, and the
	//    freed leases). Killing arbitrary DESCENDANT PROCESSES the dead session may
	//    have spawned is OUT OF SCOPE here: the daemon does not track the session's PID
	//    tree, and reaping unrelated PIDs is unsafe. Such children are reaped by the OS
	//    (re-parented to init) and, deprived of their worktree + governed leases, can no
	//    longer interfere; the launcher's graceful drop path (pty.go) is what relays the
	//    signal to the immediate child in the common case. Teardown is fail-soft (an
	//    error is accumulated, never panicked, and never blocks the scan — the lease is
	//    already freed regardless), so a stubborn boundary never strands the pass.
	if r.bound != nil {
		for h := range deathSignal {
			if live[h] {
				r.audit(h, "liveness.skipped_live_holder", jpayload(`{"holder":%q}`, h))
				continue
			}
			if err := r.bound.Teardown(h); err != nil {
				errs = errors.Join(errs, err) // non-fatal: the lease is already freed
				continue
			}
			rep.TornDown++
			r.audit(h, "liveness.reclaimed_boundary", jpayload(`{"holder":%q}`, h))
		}
	}

	// 5) RECLAIM STALE REVIEW CLAIMS: a review request stranded in `claimed` by an
	//    integrator that crashed mid-review is reverted to `requested` so a live
	//    integrator can re-claim it — no request is stuck forever behind a dead
	//    claimer. DEAD is the SAME authority used above: a claimer holding NO live
	//    lease (Inv 17). A claimer still holding ANY live lease is ALIVE and is never
	//    reclaimed, so a slow-but-live integrator never loses its claim. Reuses the
	//    `live` set already computed (the single death oracle); the CAS lives in the
	//    integration plane. Best-effort: an error is accumulated, never short-circuited.
	if r.intReclaim != nil {
		n, err := r.intReclaim.ReclaimStaleClaims(func(session string) bool { return !live[session] })
		if err != nil {
			errs = errors.Join(errs, err)
		}
		rep.ReclaimedClaims = n
		if n > 0 {
			r.audit("", "liveness.reclaimed_claims", jpayload(`{"count":%d}`, n))
		}

		// 6) GARBAGE-COLLECT aged-out review records: terminal verdicts
		//    (approved/withdrawn) and long-abandoned requests (requested/
		//    changes_requested) past their TTL are deleted so the record store does not
		//    grow without bound. This is NOT death-gated — coldness (updated_at age),
		//    not holder liveness, is the trigger — and it NEVER touches a `claimed`
		//    (in-flight) row. Best-effort: an error is accumulated, never
		//    short-circuited, so a GC failure never strands the rest of the pass.
		gced, gerr := r.intReclaim.GCStale()
		if gerr != nil {
			errs = errors.Join(errs, gerr)
		}
		rep.GCedRecords = gced
		if gced > 0 {
			r.audit("", "liveness.gc_records", jpayload(`{"count":%d}`, gced))
		}
	}

	for h := range deathSignal {
		if !live[h] {
			rep.DeadHolders = append(rep.DeadHolders, h)
		}
	}
	return rep, errs
}

func (r *Recoverer) liveSet() (map[string]bool, error) {
	live := map[string]bool{}
	hs, err := r.leases.LiveHolders()
	if err != nil {
		return live, err
	}
	for _, h := range hs {
		live[h] = true
	}
	return live, nil
}

// Loop runs Scan every interval until ctx is cancelled. It runs ONE scan
// immediately (the restart-reattachment pass: a holder that died while the daemon
// was down left an expired lease in the durable ledger, reclaimed here on start).
func (r *Recoverer) Loop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	_, _ = r.Scan()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = r.Scan()
		}
	}
}

func jpayload(format string, args ...any) []byte {
	return []byte(fmt.Sprintf(format, args...))
}
