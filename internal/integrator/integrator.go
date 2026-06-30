// Package integrator implements project 6 (integrator-trunk): the convergent
// write path. It is the SOLE promoter of the canonical trunk — the lone party
// with a path to real trunk — and it advances trunk only via a validated,
// lease-gated, idempotent, transactional promote.
//
// Owns (docs/0003 clause map): 5-integrator (single trunk promoter), 6 (the
// validated-gate half — "nothing merges silently"; the single-writer half is
// discharged by the lease, not a private mutex), 7 (trunk advances only via
// validated integration; no agent reaches real origin), 12-output (the
// load-bearing output side of the closed loop).
//
// Boundaries (what this package is NOT): it does not store leases (it CONSUMES
// the ledger's CAS via the LeaseGate — single-writer is the LEASE), it holds no
// richer/LLM validation (that is the Layer-2 gate seam, cut empty here), it is
// not the ledger arbiter, and it never renders state. Critically it NEVER
// imports liveness-recovery (project 8): liveness depends on THIS, one-way —
// `Abort(id)` is a standalone idempotent primitive liveness invokes, never a
// back-edge.
//
// "By construction" (Inv 6/7): trunk advances ONLY at a single atomic git
// update-ref compare-and-swap (see trunk.go). Git's ref store is the authority
// for the outcome — trunk == merge_commit ⟺ promoted — so a death at ANY earlier
// point leaves trunk byte-identical and Abort/recovery read the ref to decide.
package integrator

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LeaseGate is the trunk single-writer mechanism: the deterministic CAS lock the
// integrator CONSUMES (it is the LEASE, never a private mutex — docs/0004 card 6
// top risk). The composition root adapts the lease ledger to this interface so
// the integrator package never imports the lease store. Acquire is CAS-fail-fast
// (no wait/queue): a live conflict returns granted=false immediately with the
// current holder, which the integrator surfaces as a retryable "trunk busy".
type LeaseGate interface {
	Acquire(key []byte, holder string, ttl time.Duration) (granted bool, currentHolder string, err error)
	Release(key []byte, holder string) (bool, error)
}

// AuditFunc emits a decision-audit record through the daemon's audit interface
// (the composition root provides one backed by the durable sink; nil → no-op).
// Keeping it a closure means this package never couples to the daemon type and
// stays unit-testable with a spy.
type AuditFunc func(session, kind string, payload []byte)

// Integrator is the single trunk promoter. It is constructed once and hosted in
// the single arbiter daemon (Inv 5), so its store + git plumbing need no
// cross-process coordination beyond the trunk lease.
type Integrator struct {
	repo     *trunkRepo
	store    *store
	leases   LeaseGate
	trunkKey []byte
	gate     ValidationGate
	audit    AuditFunc
	clock    Clock
	leaseTTL time.Duration

	// fault is a TEST-ONLY injection point (nil in production): it is invoked at
	// each named state boundary and, when it returns an error, the operation
	// returns immediately — simulating a process death at exactly that point so a
	// test can assert trunk cleanliness + abort/recovery behavior.
	fault func(point string) error
}

// Outcome reports the result of a promote/abort/status call.
type Outcome struct {
	ID        string
	State     State
	Promoted  bool   // trunk now equals this integration's merge commit
	TrunkTip  string // trunk tip after the call (empty if unborn)
	Reason    string // gate-reject / conflict / lease-busy detail
	Retryable bool   // lease busy / trunk advanced — the caller may retry
}

var (
	// ErrNotFound — promote/status of an unknown integration id.
	ErrNotFound = errors.New("integrator: no such integration")
	// ErrAborted — promote of an already-aborted integration.
	ErrAborted = errors.New("integrator: integration already aborted")
	// ErrUnauthorized — a non-holder tried to abort another session's in-flight
	// integration via the holder-bound RPC path (Inv 4).
	ErrUnauthorized = errors.New("integrator: not the integration holder")
)

// Options configures an Integrator.
type Options struct {
	TrunkDir    string         // the trunk git directory (bare mediated repo in production)
	TrunkBranch string         // the trunk branch ref name (e.g. "main")
	StorePath   string         // integrator state DB path ("" → in-memory)
	Leases      LeaseGate      // REQUIRED: the trunk-lease CAS
	TrunkKey    []byte         // REQUIRED: the convergent lease key (manifest.TrunkKey)
	Gate        ValidationGate // nil → the v1 deterministic mergeGate
	Audit       AuditFunc      // nil → no-op
	Clock       Clock          // nil → systemClock
	LeaseTTLMs  int64          // promote-lease TTL (default 120000)
}

// New constructs an Integrator. It opens (creating if needed) the durable state
// store. The caller closes it with Close.
func New(opts Options) (*Integrator, error) {
	if opts.Leases == nil {
		return nil, errors.New("integrator: a LeaseGate is required (single-writer is the lease)")
	}
	if len(opts.TrunkKey) == 0 {
		return nil, errors.New("integrator: a trunk lease key is required")
	}
	if opts.TrunkDir == "" || opts.TrunkBranch == "" {
		return nil, errors.New("integrator: trunk dir and branch are required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	st, err := openStore(opts.StorePath, clock)
	if err != nil {
		return nil, err
	}
	gate := opts.Gate
	if gate == nil {
		gate = mergeGate{}
	}
	ttl := time.Duration(opts.LeaseTTLMs) * time.Millisecond
	if ttl <= 0 {
		ttl = 120 * time.Second
	}
	audit := opts.Audit
	if audit == nil {
		audit = func(string, string, []byte) {}
	}
	return &Integrator{
		repo:     newTrunkRepo(opts.TrunkDir, opts.TrunkBranch),
		store:    st,
		leases:   opts.Leases,
		trunkKey: append([]byte(nil), opts.TrunkKey...),
		gate:     gate,
		audit:    audit,
		clock:    clock,
		leaseTTL: ttl,
	}, nil
}

// Close releases the durable store.
func (it *Integrator) Close() error { return it.store.close() }

func (it *Integrator) fire(point string) error {
	if it.fault == nil {
		return nil
	}
	return it.fault(point)
}

// Submit records a new integration request for branch (a ref the agent pushed /
// a worktree branch) and returns it in `received`. session is the daemon's
// connection-bound identity (Inv 4); it becomes the integration holder. Submit
// makes NO trunk change — it only captures the current trunk tip as the base.
func (it *Integrator) Submit(session, branch string) (Record, error) {
	if branch == "" {
		return Record{}, errors.New("integrator: branch required")
	}
	// The branch ref is client-controlled (RPC params). Reject anything that is
	// not a plain ref BEFORE it reaches a git command line, so a value like
	// "--upload-pack=…" or "-x" can never be parsed as a git option (arg
	// injection). The resolve below additionally uses --end-of-options.
	if !validRef(branch) {
		return Record{}, fmt.Errorf("integrator: invalid branch ref %q", branch)
	}
	if _, err := it.repo.resolve(branch); err != nil {
		return Record{}, fmt.Errorf("integrator: branch %q does not resolve: %w", branch, err)
	}
	base, _, err := it.repo.tip()
	if err != nil {
		return Record{}, err
	}
	id, err := newIntegrationID()
	if err != nil {
		return Record{}, err
	}
	rec, _, err := it.store.insertReceived(id, branch, session, base)
	if err != nil {
		return Record{}, err
	}
	it.audit(session, "trunk.submitted", []byte(fmt.Sprintf(`{"id":%q,"branch":%q}`, id, branch)))
	return rec, nil
}

// Promote drives an integration to `promoted`: validate (mandatory gate) → write
// the merge commit object → acquire the trunk lease (CAS-fail-fast single-writer)
// → atomic update-ref CAS → record promoted. It is IDEMPOTENT: re-promoting a
// promoted id is a no-op that returns promoted; an aborted id errors. session is
// the promoting caller's connection-bound identity — the lease is acquired under
// it, so a second concurrent promoter (different session) is rejected by the CAS.
func (it *Integrator) Promote(session, id string) (Outcome, error) {
	rec, ok, err := it.store.get(id)
	if err != nil {
		return Outcome{}, err
	}
	if !ok {
		return Outcome{ID: id, Reason: "no such integration"}, ErrNotFound
	}

	// Terminal states first — reconcile against git (the authority).
	if eff, done, out, rerr := it.reconcileTerminal(rec); done {
		return out, rerr
	} else {
		rec = eff
	}

	// received -> validating: run the MANDATORY gate, write the merge commit
	// object (no ref moves), record validating+gate+merge-commit atomically.
	if rec.State == StateReceived {
		trunkTip, _, err := it.repo.tip()
		if err != nil {
			return Outcome{}, err
		}
		branchOID, err := it.repo.resolve(rec.Branch)
		if err != nil {
			// The branch vanished — a client fault, not a trunk hazard. Abort it.
			_, _ = it.store.cas(id, StateReceived, StateAborted)
			return Outcome{ID: id, State: StateAborted, Reason: "branch unresolved: " + err.Error()}, nil
		}
		gr, err := it.gate.Validate(it.repo, trunkTip, branchOID)
		if err != nil {
			return Outcome{}, err
		}
		if !gr.OK {
			// Validation failed → NEVER promote. Terminal aborted; trunk untouched.
			_, _ = it.store.cas(id, StateReceived, StateAborted)
			it.audit(session, "trunk.rejected", []byte(fmt.Sprintf(`{"id":%q,"reason":%q}`, id, gr.Reason)))
			return Outcome{ID: id, State: StateAborted, Reason: "validation failed: " + gr.Reason}, nil
		}
		merge, err := it.repo.commitMerge(gr.Tree, trunkTip, branchOID, it.mergeMessage(id, rec.Branch), rec.CreatedAt)
		if err != nil {
			return Outcome{}, err
		}
		// The merge commit object now exists but NO ref points at it — a death here
		// leaves trunk byte-identical and the object dangling (GC'd later). The
		// transition is a CAS: if a concurrent same-id Promote won the race, ours
		// returns ok=false and we adopt the DURABLE record (its merge commit is
		// deterministic-identical to ours, since commitMerge is pinned per id).
		ok, err := it.store.setValidating(id, trunkTip, merge, true, "")
		if err != nil {
			return Outcome{}, err
		}
		if !ok {
			if cur, found, gerr := it.store.get(id); gerr != nil {
				return Outcome{}, gerr
			} else if found {
				rec = cur
			}
		} else {
			rec.State, rec.MergeCommit, rec.Base, rec.GateOK = StateValidating, merge, trunkTip, true
		}
		if err := it.fire("after-validating"); err != nil {
			return Outcome{ID: id, State: StateValidating, Reason: "fault:after-validating"}, err
		}
	}

	// rec.State == validating, with Base + MergeCommit set.
	// SINGLE-WRITER: acquire the trunk lease (CAS-fail-fast). Refusing here when
	// the lease is unavailable IS the "no promote without the lease" guarantee.
	granted, holder, err := it.leases.Acquire(it.trunkKey, session, it.leaseTTL)
	if err != nil {
		return Outcome{}, err
	}
	if !granted {
		return Outcome{ID: id, State: StateValidating, Reason: "trunk busy, held by " + holder, Retryable: true}, nil
	}
	defer it.leases.Release(it.trunkKey, session)
	if err := it.fire("after-lease"); err != nil {
		return Outcome{ID: id, State: StateValidating, Reason: "fault:after-lease"}, err
	}

	// Re-load the DURABLE record under the held lease: it is the authority for
	// Base/MergeCommit. A concurrent same-id Promote may have persisted the row;
	// advancing trunk to the durably recorded commit (never a stale in-memory OID)
	// keeps merge_commit == the promoted ref.
	if cur, found, gerr := it.store.get(id); gerr != nil {
		return Outcome{}, gerr
	} else if found {
		rec = cur
	}

	// Already landed (possibly SUPERSEDED by a later promote since)? Reconcile to
	// promoted by ANCESTRY, not exact-tip equality — once our merge commit is an
	// ancestor of trunk it stays one, even after the next integration advances
	// trunk past it. This is the crux crash-consistency fix.
	if done, err := it.landed(rec); err != nil {
		return Outcome{}, err
	} else if done {
		_, _ = it.store.cas(id, StateValidating, StatePromoted)
		tip, _, _ := it.repo.tip()
		return Outcome{ID: id, State: StatePromoted, Promoted: true, TrunkTip: tip}, nil
	}

	nowTip, _, err := it.repo.tip()
	if err != nil {
		return Outcome{}, err
	}
	if nowTip != rec.Base {
		// Trunk advanced under us since validation AND our merge commit is not an
		// ancestor (checked above) → genuinely stale. Abort cleanly; the caller
		// resubmits against the new tip. Trunk is untouched. (v1: no auto-rebase.)
		_, _ = it.store.cas(id, StateValidating, StateAborted)
		return Outcome{ID: id, State: StateAborted, Reason: "trunk advanced since validation; resubmit", Retryable: true}, nil
	}

	if err := it.fire("before-updateref"); err != nil {
		// Death here: trunk still == Base (clean). Abort(id) is safe.
		return Outcome{ID: id, State: StateValidating, Reason: "fault:before-updateref"}, err
	}

	// THE SINGLE ATOMIC MUTATION (the whole "by construction" guarantee).
	if err := it.repo.advanceTrunk(rec.MergeCommit, baseArg(rec.Base)); err != nil {
		// The CAS failed (trunk moved) → trunk untouched; stay validating, retryable.
		return Outcome{ID: id, State: StateValidating, Reason: "update-ref CAS failed: " + err.Error(), Retryable: true}, nil
	}
	if err := it.fire("after-updateref"); err != nil {
		// Death here: trunk == MergeCommit (promoted atomically) but the store row is
		// still `validating`. Recovery/Abort read the ref and reconcile to promoted.
		return Outcome{ID: id, State: StateValidating, Promoted: true, TrunkTip: rec.MergeCommit, Reason: "fault:after-updateref"}, err
	}

	_, _ = it.store.cas(id, StateValidating, StatePromoted)
	it.audit(session, "trunk.promoted", []byte(fmt.Sprintf(`{"id":%q,"branch":%q,"commit":%q}`, id, rec.Branch, rec.MergeCommit)))
	return Outcome{ID: id, State: StatePromoted, Promoted: true, TrunkTip: rec.MergeCommit}, nil
}

// Abort transitions a non-terminal integration to `aborted` — the idempotent
// primitive liveness-recovery invokes on a dead mid-integration holder. It
// NEVER moves a ref: trunk is clean "by construction" because the atomic
// update-ref runs only inside Promote. It reconciles against git first: if the
// atomic advance already landed (trunk == merge commit), the integration is
// PROMOTED and abort is a terminal no-op — an abort can never undo a completed,
// atomic promote. Idempotent: aborting an aborted/absent id is a safe no-op.
func (it *Integrator) Abort(id string) (Outcome, error) {
	rec, ok, err := it.store.get(id)
	if err != nil {
		return Outcome{}, err
	}
	if !ok {
		return Outcome{ID: id, Reason: "no such integration"}, nil // idempotent
	}
	if rec.State == StateAborted {
		return Outcome{ID: id, State: StateAborted}, nil
	}
	if rec.State == StatePromoted {
		tip, _, _ := it.repo.tip()
		return Outcome{ID: id, State: StatePromoted, Promoted: true, TrunkTip: tip}, nil
	}
	// received | validating: reconcile against git by ANCESTRY — did the atomic
	// advance land (and possibly get superseded since)? If our merge commit is
	// reachable from trunk, the integration is PROMOTED and abort never undoes it.
	if done, derr := it.landed(rec); derr != nil {
		return Outcome{}, derr
	} else if done {
		_, _ = it.store.cas(id, rec.State, StatePromoted)
		tip, _, _ := it.repo.tip()
		return Outcome{ID: id, State: StatePromoted, Promoted: true, TrunkTip: tip}, nil
	}
	_, _ = it.store.cas(id, rec.State, StateAborted)
	tip, _, _ := it.repo.tip()
	it.audit(rec.Holder, "trunk.aborted", []byte(fmt.Sprintf(`{"id":%q}`, id)))
	return Outcome{ID: id, State: StateAborted, TrunkTip: tip}, nil
}

// AbortAs is the HOLDER-BOUND abort for the RPC surface (Inv 4): only the
// integration's own holder (cc.Session) may cancel an in-flight integration, so
// one session cannot abort another's (a cross-session DoS on the convergent write
// path). Terminal or already-landed integrations are idempotent no-ops needing
// no authz (nothing in-flight to cancel). The privileged, NON-holder-bound
// Abort(id) — which liveness invokes on a DEAD holder whose session is gone — is
// intentionally separate.
func (it *Integrator) AbortAs(session, id string) (Outcome, error) {
	rec, ok, err := it.store.get(id)
	if err != nil {
		return Outcome{}, err
	}
	if !ok {
		return Outcome{ID: id, Reason: "no such integration"}, nil // idempotent
	}
	if rec.State == StateAborted || rec.State == StatePromoted {
		return it.Abort(id) // terminal: idempotent, no authz needed
	}
	if done, derr := it.landed(rec); derr == nil && done {
		return it.Abort(id) // already landed → promoted no-op, no authz needed
	}
	if rec.Holder != session {
		return Outcome{ID: id, State: rec.State}, ErrUnauthorized
	}
	return it.Abort(id)
}

// Status returns the reconciled state of an integration.
func (it *Integrator) Status(id string) (Outcome, error) {
	rec, ok, err := it.store.get(id)
	if err != nil {
		return Outcome{}, err
	}
	if !ok {
		return Outcome{ID: id}, ErrNotFound
	}
	eff, _, out, _ := it.reconcileTerminal(rec)
	if out.State != "" {
		return out, nil
	}
	tip, _, _ := it.repo.tip()
	return Outcome{ID: id, State: eff.State, TrunkTip: tip}, nil
}

// InFlight returns every non-terminal integration — the set liveness scans on a
// holder death / daemon restart to fire idempotent aborts (project 8 consumes
// this; the integrator never imports liveness).
func (it *Integrator) InFlight() ([]Record, error) { return it.store.inFlight() }

// List returns all integration records (diagnostics / the watch surface).
func (it *Integrator) List() ([]Record, error) { return it.store.list() }

// TrunkTip returns the current trunk commit (read-only; for diagnostics/tests).
func (it *Integrator) TrunkTip() (string, bool, error) { return it.repo.tip() }

// TrunkBranchName returns the trunk branch ref name (read-only; surfaced to the
// watch view's trunk panel via integrate.trunk).
func (it *Integrator) TrunkBranchName() string { return it.repo.branch }

// reconcileTerminal resolves a record against git's authoritative ref. If it is
// terminal (promoted/aborted) it returns done=true with the Outcome; if the
// atomic advance already landed for a still-`validating` row it reconciles to
// promoted. Otherwise it returns the (possibly updated) record with done=false.
func (it *Integrator) reconcileTerminal(rec Record) (eff Record, done bool, out Outcome, err error) {
	switch rec.State {
	case StatePromoted:
		tip, _, _ := it.repo.tip()
		return rec, true, Outcome{ID: rec.ID, State: StatePromoted, Promoted: true, TrunkTip: tip}, nil
	case StateAborted:
		return rec, true, Outcome{ID: rec.ID, State: StateAborted, Reason: "integration aborted"}, ErrAborted
	}
	if rec.State == StateValidating {
		if done, derr := it.landed(rec); derr == nil && done {
			_, _ = it.store.cas(rec.ID, StateValidating, StatePromoted)
			rec.State = StatePromoted
			tip, _, _ := it.repo.tip()
			return rec, true, Outcome{ID: rec.ID, State: StatePromoted, Promoted: true, TrunkTip: tip}, nil
		}
	}
	return rec, false, Outcome{}, nil
}

// landed reports whether this integration's merge commit is part of trunk now —
// i.e. its atomic advance landed (and may have been superseded by a later
// promote since). It tests ANCESTRY, not exact-tip equality: trunk only
// advances, so once the merge commit is an ancestor of trunk it stays one. The
// merge commit OID is UNIQUE per integration (the message embeds the id and the
// date is pinned to CreatedAt), so reachability uniquely identifies THIS
// integration's landed advance.
func (it *Integrator) landed(rec Record) (bool, error) {
	if rec.MergeCommit == "" {
		return false, nil
	}
	tip, present, err := it.repo.tip()
	if err != nil || !present {
		return false, err
	}
	return it.repo.isAncestor(rec.MergeCommit, tip)
}

func (it *Integrator) mergeMessage(id, branch string) string {
	return fmt.Sprintf("mad-substrate: integrate %s\n\nintegration-id: %s", branch, id)
}

// baseArg maps an empty base (unborn trunk) to the create-only CAS sentinel.
func baseArg(base string) string { return base }

func newIntegrationID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "int-" + hex.EncodeToString(b[:]), nil
}

// validRef reports whether s is a safe git ref string to place on a command
// line: non-empty, not option-like (no leading '-'), only ref-legal characters,
// and free of the rev-spec metacharacters ('^', '~', ':', '?', '*', '[', '\',
// whitespace) and the ".." range / "@{" reflog syntax — so it cannot be parsed
// as a git option or expand into an unintended revision.
func validRef(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "@{") {
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
