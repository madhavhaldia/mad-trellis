// Package integration implements the daemon-side review/verdict plane (Wing 3):
// a convergence-scoped record + state-machine queue an external integrator agent
// drives. A BUILDER agent commits work on its boundary branch and REQUESTS
// integration; an INTEGRATOR agent lists PENDING requests, CLAIMS one, reviews,
// and records a VERDICT (approve or reject-with-feedback); the builder polls
// STATUS and iterates.
//
// This plane is PURE records + a state machine — it performs NO git and NO merge.
// The actual merge happens elsewhere (the integrator-trunk plane); on approve the
// caller passes the commit OID it already produced, recorded here only so the
// builder's status poll can observe it. State lives in a single-writer SQLite
// ledger with atomic CAS transitions, mirroring internal/integrator.
//
// Identity (Inv 4): every mutating operation takes the caller's connection-bound
// session id EXPLICITLY (the RPC layer passes cc.Session, never a value from
// params). Holder is the requesting builder; Claimer is the claiming integrator.
package integration

import (
	"errors"
	"strings"
	"time"
)

// Default GC retentions. Terminal rows (approved/withdrawn) carry a landed verdict
// and are dropped quickly; abandoned rows (requested/changes_requested) get a LONG
// grace because a builder requesting then EXITING for async review is NORMAL — this
// is a TTL on coldness, never a reclaim-on-builder-death.
const (
	defaultTerminalRetention  = 1 * time.Hour
	defaultAbandonedRetention = 24 * time.Hour
)

// Errors surfaced to the RPC layer (mapped to protocol codes there).
var (
	// ErrInvalidBranch — the branch is not a safe ref string.
	ErrInvalidBranch = errors.New("integration: invalid branch ref")
	// ErrFeedbackRequired — a reject verdict carried no feedback.
	ErrFeedbackRequired = errors.New("integration: feedback required to reject")
	// ErrBadDecision — verdict decision was neither approve nor reject.
	ErrBadDecision = errors.New("integration: decision must be approve or reject")
)

// Integration wraps the durable store with the five logical operations the RPC
// surface exposes. It is constructed once and hosted in the single arbiter daemon
// (Inv 5), so the single-writer store needs no cross-process coordination.
type Integration struct {
	store *store
}

// Options configures an Integration.
type Options struct {
	StorePath string // request-ledger DB path ("" → in-memory)
	Clock     Clock  // nil → systemClock
}

// New constructs an Integration, opening (creating if needed) its durable store.
// The caller closes it with Close.
func New(opts Options) (*Integration, error) {
	st, err := openStore(opts.StorePath, opts.Clock)
	if err != nil {
		return nil, err
	}
	return &Integration{store: st}, nil
}

// Close releases the durable store.
func (ig *Integration) Close() error { return ig.store.close() }

// Request records (or re-records) a builder's request to integrate branch,
// landing it in `requested` with the requesting session as holder. It is an
// UPSERT keyed by branch: a re-request after revising RESETS an existing row back
// to `requested`, clearing any prior claimer + feedback. An already-approved or
// absent branch is (re)created as `requested`. session is the connection-bound
// builder identity (Inv 4). The branch must be a safe ref.
func (ig *Integration) Request(session, branch, title string) (Record, error) {
	if !validRef(branch) {
		return Record{}, ErrInvalidBranch
	}
	return ig.store.upsertRequest(branch, title, session)
}

// Pending lists every request still in `requested` (the integrator's work
// queue), oldest first.
func (ig *Integration) Pending() ([]Record, error) { return ig.store.pending() }

// Claim attempts to CAS branch from requested -> claimed, recording session as
// the claimer. ok=false (not an error) when the row is not in `requested` (a
// second integrator lost the race, or the builder hasn't requested). On a
// successful claim it returns the claimed record so the integrator gets the
// title to review.
func (ig *Integration) Claim(session, branch string) (ok bool, rec Record, err error) {
	ok, err = ig.store.claim(branch, session)
	if err != nil {
		return false, Record{}, err
	}
	if !ok {
		// Return the current record (if any) so the caller can render branch/title
		// even on a lost race; ok=false is the authoritative signal.
		cur, _, gerr := ig.store.get(branch)
		return false, cur, gerr
	}
	cur, _, gerr := ig.store.get(branch)
	return true, cur, gerr
}

// Verdict records the integrator's decision on a CLAIMED request.
//
//   - reject: feedback is REQUIRED; CAS claimed -> changes_requested, storing it.
//   - approve: CAS claimed -> approved, storing merge (the commit OID the caller
//     already produced).
//
// ok=false (not an error) when the CAS predicate didn't hold (the row was not in
// `claimed`). session is accepted for symmetry/audit but the verdict is not
// holder-bound beyond the claim gate. Returns the resulting state.
func (ig *Integration) Verdict(session, branch, decision, feedback, merge string) (ok bool, state State, err error) {
	switch decision {
	case "approve":
		ok, err = ig.store.approve(branch, merge)
		if err != nil {
			return false, "", err
		}
		if ok {
			return true, StateApproved, nil
		}
	case "reject":
		if strings.TrimSpace(feedback) == "" {
			return false, "", ErrFeedbackRequired
		}
		ok, err = ig.store.reject(branch, feedback)
		if err != nil {
			return false, "", err
		}
		if ok {
			return true, StateChangesRequested, nil
		}
	default:
		return false, "", ErrBadDecision
	}
	// CAS predicate failed — report the current state so the caller sees why.
	cur, found, gerr := ig.store.get(branch)
	if gerr != nil {
		return false, "", gerr
	}
	if !found {
		return false, "", nil
	}
	return false, cur.State, nil
}

// Status returns the current record for branch (the builder's poll).
func (ig *Integration) Status(branch string) (Record, bool, error) {
	return ig.store.get(branch)
}

// Cancel withdraws an IN-FLIGHT request (requested or claimed) for branch to the
// terminal `withdrawn` state, clearing it from the integrator's queue. ok=false
// (not an error) when the row is absent or already in a terminal/verdict state —
// a cancel never clobbers a landed approve/reject.
func (ig *Integration) Cancel(branch string) (bool, error) {
	return ig.store.cancel(branch)
}

// ReclaimStaleClaims reverts every `claimed` row whose claimer is dead (per
// isDead) back to `requested`, clearing the dead claimer + feedback, and returns
// the count reverted. A live claimer is never reclaimed. This is the liveness
// sweep's hook: a crashed integrator no longer strands a record in `claimed`.
func (ig *Integration) ReclaimStaleClaims(isDead func(session string) bool) (int, error) {
	return ig.store.reclaimStaleClaims(isDead)
}

// GCStale garbage-collects aged-out records using the package default retentions:
// terminal rows (approved/withdrawn) older than defaultTerminalRetention and
// abandoned rows (requested/changes_requested) older than defaultAbandonedRetention.
// A `claimed` row is never collected (it is in-flight; the liveness stale-claim
// sweep handles a dead claimer). It returns the count deleted. This is the liveness
// scan's GC hook — best-effort, idempotent, and never reclaim-on-builder-death.
func (ig *Integration) GCStale() (int, error) {
	return ig.store.gc(ig.store.clock.Now(), defaultTerminalRetention, defaultAbandonedRetention)
}

// List returns EVERY record in any state, newest-updated first. Read-only (the
// watch surface's whole-queue view).
func (ig *Integration) List() ([]Record, error) {
	return ig.store.list()
}

// validRef reports whether s is a safe boundary-branch ref: non-empty, not
// option-like (no leading '-'), and only the ref-legal characters
// [A-Za-z0-9._/-]. This is a pure record key here (no git command line is built
// from it), but the plane still refuses an unsafe ref so the same value is safe
// for the downstream merge plane to consume.
func validRef(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
