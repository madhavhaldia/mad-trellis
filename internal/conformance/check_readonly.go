package conformance

import (
	"fmt"
	"sort"
	"strings"
)

// check_readonly.go proves the READ-ONLY-SURFACE clause (docs/0003 §10a; Inv
// 13-readonly + Inv 12-readsurface, owned by watch-view-surface, project 9a): the
// only new surface of the closed loop is READ-ONLY. A scenario snapshot of
// governed state is UNCHANGED after driving the read-only watch surface, and the
// surface reaches NO mutating method.
//
// BLACK BOX over the public surface only. The watch view (`mad-trellis watch`)
// reaches the daemon through EXACTLY this read set: diag.health, session.whoami,
// audit.tail, lease.list, integrate.list, integrate.trunk, integration.list. The
// probe:
//   1. drives a real scenario to a non-trivial governed state (a born trunk + a
//      held lease + audit records), then SNAPSHOTS that state via raw observable
//      reads (the trunk ref, the lease holder, the integration list);
//   2. drives the FULL read-only watch surface (every read method the view calls),
//      repeatedly;
//   3. re-snapshots and asserts the governed state is BYTE-IDENTICAL — the read
//      surface mutated nothing.
//
// CONTROL (non-vacuity): the snapshot must be able to OBSERVE a real change. The
// control performs an actual mutation (advance the trunk via a legit promote)
// between two snapshots and asserts the snapshot DIFFERS — proving the equality
// check in Run is meaningful (it is not blind to state changes).

func init() { RegisterCheck(readOnlySurface{}) }

type readOnlySurface struct{}

func (readOnlySurface) ID() string           { return "read-only-surface" }
func (readOnlySurface) OwnerProject() string { return "watch-view-surface" }
func (readOnlySurface) Clause() string {
	return "read-only surface (9a): driving the watch read surface leaves governed state unchanged; it reaches no mutating method (Inv 13-readonly/12-readsurface)"
}

// watchReadMethods is the EXACT set of methods the read-only watch view reaches
// (mirrors internal/watch's client fetcher). Every one must be non-mutating.
//
// #18 — DRIFT NOTE: this list is HAND-MIRRORED from internal/watch. The PRIMARY
// guard against the watch view gaining a mutating call is project 9a's OWN
// ship-time grep test over internal/watch (it asserts the fetcher reaches only this
// read set) — that lives with the code it guards and cannot drift from it. THIS
// list is the conformance restatement: if 9a adds a read method, mirror it here so
// the snapshot probe drives the full surface. We deliberately do NOT reflect/derive
// it across the package boundary (the black-box rule forbids importing internal/
// watch); the 9a grep is the authoritative non-mutation guard, this is the E2E
// "driving the whole read surface mutates nothing" check.
var watchReadMethods = []string{
	"diag.health",
	"session.whoami",
	"audit.tail",
	"lease.list",
	"integrate.list",
	"integrate.trunk",
	"integration.list",
}

// governedSnapshot is the observable governed state the read surface must not move.
// #13: it is BROADENED beyond 3 scalars so a mutation via a read method — an
// audit-log append, a lease acquire/renew (TTL/holder change), or an integration
// state transition — cannot be invisible to the equality check. It captures the
// trunk tip, an audit-log fingerprint (count + newest record), a per-lease
// holder+TTL fingerprint, and a per-integration state fingerprint.
type governedSnapshot struct {
	trunkTip      string
	auditFP       string // count + newest audit record (catches an audit append)
	leaseFP       string // sorted key->holder@expires_ms (catches a lease/TTL/holder change)
	integrationFP string // sorted id->state (catches an integration state transition)
}

func (c readOnlySurface) Run(s *Scratch) Result {
	agent, err := s.NewAgent("ro")
	if err != nil {
		return fail(c, "new agent: %v", err)
	}
	// Drive a non-trivial governed state: born trunk.
	base, err := s.EstablishTrunkBase(agent, map[string]string{"a.txt": "base\n"})
	if err != nil {
		return fail(c, "establish trunk base: %v", err)
	}
	if base == "" {
		return fail(c, "trunk did not come into being")
	}
	// Hold the trunk lease so lease.list has a holder to observe.
	key, ok, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !ok {
		return fail(c, "route trunk key: ok=%v err=%v", ok, err)
	}
	holder, err := s.Dial()
	if err != nil {
		return fail(c, "holder dial: %v", err)
	}
	defer holder.Close()
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := holder.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &acq); err != nil {
		return fail(c, "hold lease: %v", err)
	}
	if !acq.Granted {
		return fail(c, "could not hold the trunk lease for the snapshot")
	}

	before, err := c.snapshot(s)
	if err != nil {
		return fail(c, "snapshot before: %v", err)
	}

	// Drive the FULL read-only watch surface several times (every read method the
	// view calls), exactly as the TUI would on its poll interval.
	for rep := 0; rep < 3; rep++ {
		if err := c.driveWatchReads(s); err != nil {
			return fail(c, "drive watch reads rep %d: %v", rep, err)
		}
	}

	after, err := c.snapshot(s)
	if err != nil {
		return fail(c, "snapshot after: %v", err)
	}
	if before != after {
		return fail(c, "MUTATION VIA READ SURFACE: governed state changed across the read-only watch surface: before=%+v after=%+v", before, after)
	}

	return pass(c, "read-only watch surface (%v) left governed state BYTE-IDENTICAL across trunk tip + audit-log + per-lease TTL/holder + per-integration state: trunk %s, audit %q",
		watchReadMethods, short12(after.trunkTip), after.auditFP)
}

func (c readOnlySurface) Control(s *Scratch) error {
	// Non-vacuity: the BROADENED snapshot must DETECT a real governed mutation along an
	// ADDED dimension (#13). We exercise TWO added dimensions so the broadening is
	// genuinely non-vacuous, not just the trunk tip:
	//
	//   (i) an AUDIT-LOG append (the audit dimension): a mutating write the old
	//       3-scalar snapshot was blind to; AND
	//   (ii) a LEASE acquire (the per-lease holder/TTL dimension).
	//
	// If either real mutation leaves the snapshot equal, the equality check is blind to
	// that dimension and the Run's "unchanged" verdict proves nothing for it.
	agent, err := s.NewAgent("ro-ctl")
	if err != nil {
		return fmt.Errorf("control new agent: %w", err)
	}
	if _, err := s.EstablishTrunkBase(agent, map[string]string{"a.txt": "base\n"}); err != nil {
		return fmt.Errorf("control establish trunk: %w", err)
	}

	// (i) AUDIT dimension: snapshot, append an audit record, assert the snapshot moved.
	beforeAudit, err := c.snapshot(s)
	if err != nil {
		return fmt.Errorf("control snapshot before audit: %w", err)
	}
	au, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control audit dial: %w", err)
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	aerr := au.Call("audit.append", map[string]any{
		"decision_project": "conformance-control",
		"decision_kind":    "readonly-control-mutation",
	}, &ok)
	au.Close()
	if aerr != nil {
		return fmt.Errorf("control audit.append: %w", aerr)
	}
	afterAudit, err := c.snapshot(s)
	if err != nil {
		return fmt.Errorf("control snapshot after audit: %w", err)
	}
	if beforeAudit.auditFP == afterAudit.auditFP {
		return fmt.Errorf("CONTROL VACUOUS: the snapshot's audit fingerprint did not change after a REAL audit.append (before=%q after=%q) — the snapshot is blind to audit mutations", beforeAudit.auditFP, afterAudit.auditFP)
	}

	// (ii) LEASE dimension: acquire the trunk lease, assert the lease fingerprint moved.
	beforeLease, err := c.snapshot(s)
	if err != nil {
		return fmt.Errorf("control snapshot before lease: %w", err)
	}
	key, found, err := s.RouteLeaseKey("trunk", "")
	if err != nil || !found {
		return fmt.Errorf("control route key: ok=%v err=%v", found, err)
	}
	lh, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control lease dial: %w", err)
	}
	defer lh.Close()
	var acq struct {
		Granted bool `json:"granted"`
	}
	if err := lh.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 120000}, &acq); err != nil {
		return fmt.Errorf("control lease acquire: %w", err)
	}
	afterLease, err := c.snapshot(s)
	if err != nil {
		return fmt.Errorf("control snapshot after lease: %w", err)
	}
	if beforeLease.leaseFP == afterLease.leaseFP {
		return fmt.Errorf("CONTROL VACUOUS: the snapshot's lease fingerprint did not change after a REAL lease acquire (before=%q after=%q) — the snapshot is blind to lease/holder/TTL mutations", beforeLease.leaseFP, afterLease.leaseFP)
	}
	return nil
}

// driveWatchReads calls EVERY read method the watch view reaches, exactly as the
// fetcher does. None of these may mutate state — that is what Run asserts by
// comparing snapshots taken around these calls.
//
// FAIL-SOFT, faithfully: the watch fetcher isolates each panel, so a read method
// the daemon has not (yet) wired degrades ONLY that panel to "unavailable" — it
// does not break the surface, and a method-not-found reply mutates NOTHING. We
// therefore TOLERATE method-not-found (-32601) here (it is exactly what the watch
// client tolerates), so a read in the hand-mirrored set that a sibling wing has
// not landed in this build cannot wedge the clause. ANY OTHER error still fails
// the probe: a servable read method must be fully serviceable.
func (c readOnlySurface) driveWatchReads(s *Scratch) error {
	cl, err := s.Dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	for _, m := range watchReadMethods {
		var params map[string]any
		if m == "audit.tail" {
			params = map[string]any{"limit": 50}
		} else {
			params = map[string]any{}
		}
		var out map[string]any
		if err := cl.Call(m, params, &out); err != nil {
			if isMethodNotFound(err) {
				continue // not wired in this build: the watch panel degrades; no mutation
			}
			return fmt.Errorf("read method %q: %w", m, err)
		}
	}
	return nil
}

// snapshot captures the observable governed state via the public read surface +
// raw git on the mediated trunk. #13: it fingerprints not just COUNTS but the
// CONTENT of each dimension — trunk tip, the audit log (count + newest record), the
// per-lease holder+expiry, and the per-integration state — so an audit append, a
// lease/TTL/holder change, or an integration transition through a read method
// cannot hide behind an unchanged count.
func (c readOnlySurface) snapshot(s *Scratch) (governedSnapshot, error) {
	tip, err := s.TrunkTip()
	if err != nil {
		return governedSnapshot{}, err
	}
	cl, err := s.Dial()
	if err != nil {
		return governedSnapshot{}, err
	}
	defer cl.Close()

	// Per-lease holder@expiry fingerprint (sorted, deterministic).
	var leases struct {
		Holders []struct {
			Key       string `json:"key"`
			Holder    string `json:"holder"`
			ExpiresAt int64  `json:"expires_at_ms"`
		} `json:"holders"`
	}
	if err := cl.Call("lease.list", map[string]any{}, &leases); err != nil {
		return governedSnapshot{}, err
	}
	leaseParts := make([]string, 0, len(leases.Holders))
	for _, h := range leases.Holders {
		leaseParts = append(leaseParts, fmt.Sprintf("%s=%s@%d", h.Key, h.Holder, h.ExpiresAt))
	}
	sort.Strings(leaseParts)

	// Per-integration id->state fingerprint (sorted).
	var integ struct {
		Integrations []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"integrations"`
	}
	if err := cl.Call("integrate.list", map[string]any{}, &integ); err != nil {
		return governedSnapshot{}, err
	}
	integParts := make([]string, 0, len(integ.Integrations))
	for _, in := range integ.Integrations {
		integParts = append(integParts, in.ID+"="+in.State)
	}
	sort.Strings(integParts)

	// Audit-log fingerprint: count + the newest record's identifying fields (catches
	// an append even when the tail window is capped).
	var audit struct {
		Records []struct {
			TimestampMs     int64  `json:"timestamp_ms"`
			Session         string `json:"session"`
			DecisionProject string `json:"decision_project"`
			DecisionKind    string `json:"decision_kind"`
		} `json:"records"`
	}
	if err := cl.Call("audit.tail", map[string]any{"limit": 500}, &audit); err != nil {
		return governedSnapshot{}, err
	}
	auditFP := fmt.Sprintf("n=%d", len(audit.Records))
	if len(audit.Records) > 0 {
		r := audit.Records[0] // newest-first
		auditFP += fmt.Sprintf(";top=%d|%s|%s|%s", r.TimestampMs, r.Session, r.DecisionProject, r.DecisionKind)
	}

	return governedSnapshot{
		trunkTip:      tip,
		auditFP:       auditFP,
		leaseFP:       strings.Join(leaseParts, ","),
		integrationFP: strings.Join(integParts, ","),
	}, nil
}
