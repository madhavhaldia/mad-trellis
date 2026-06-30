package watch

// Snapshot is the immutable, per-poll view of the governance loop the model
// renders. It is the SINGLE source the View reads — the daemon is the single
// source of truth, so the model derives nothing governed beyond the latest
// snapshot (no local cache of governed state). Each panel carries its OWN
// availability: a wedged/errored call degrades only that panel to "unavailable"
// while the rest still render.
//
// Snapshot is produced by a fetch func injected into the model, so the model is
// unit-testable WITHOUT a live daemon: tests construct a Snapshot literal.
type Snapshot struct {
	// DaemonReachable is false only when even the cheapest probe (diag.health)
	// failed — the model then shows the friendly full-screen "cannot reach
	// daemon" message. A reachable daemon with one wedged sub-call keeps this
	// true and degrades the affected panel instead.
	DaemonReachable bool

	// Socket is echoed for the unreachable message.
	Socket string

	// Whoami is this watch connection's daemon-minted identity (read-only,
	// informational header). Empty when unavailable.
	Whoami string

	Trunk        TrunkPanel
	Leases       LeasePanel
	Integrations IntegrationPanel
	Reviews      ReviewPanel
	Audit        AuditPanel
}

// TrunkPanel is the integrate.trunk result plus its availability. Tip is the
// AUTHORITATIVE trunk ref (the commit the integrator's CAS last advanced), read
// directly — never derived from the integrate.list ordering.
type TrunkPanel struct {
	Available bool
	Exists    bool
	Tip       string
	Branch    string
}

// LeasePanel is the lease.list result plus its availability.
type LeasePanel struct {
	Available bool
	Holders   []LeaseHolder
}

// IntegrationPanel is the integrate.list result plus its availability. Trunk and
// pending/in-flight are DERIVED from this list; the trunk TIP is read separately
// (integrate.trunk), the promoted COUNT is tallied from this list.
type IntegrationPanel struct {
	Available    bool
	Integrations []Integration
}

// ReviewPanel is the integration.list result plus its availability: the Wing-3
// REVIEW queue (request -> claim -> verdict), surfaced read-only so the
// integration plane is observable in `watch` and not only via agent MCP tools.
type ReviewPanel struct {
	Available bool
	Records   []IntegrationRecord
}

// AuditPanel is the audit.tail result plus its availability.
type AuditPanel struct {
	Available bool
	Entries   []AuditEntry
}

// Fetcher is the function the model polls each tick to produce a Snapshot. The
// production fetcher wraps a read-only Client; tests inject a stub.
type Fetcher func() Snapshot

// NewClientFetcher builds the production Fetcher: it polls the read-only client,
// isolating each panel so one wedged call degrades only its panel. diag.health
// is the reachability probe; if it fails the whole snapshot is marked
// unreachable (the friendly full-screen path), but the per-panel calls are
// STILL attempted so a partially-wedged daemon shows whatever it can. auditLimit
// bounds the audit tail.
func NewClientFetcher(c *Client, auditLimit int) Fetcher {
	return func() Snapshot {
		s := Snapshot{Socket: c.Socket()}

		// Reachability probe (cheapest read). Its success/failure decides the
		// full-screen unreachable path.
		if _, err := c.Health(); err == nil {
			s.DaemonReachable = true
		}
		if who, err := c.Whoami(); err == nil {
			s.Whoami = who
		}

		if ref, err := c.TrunkRef(); err == nil {
			s.Trunk = TrunkPanel{Available: true, Exists: ref.Exists, Tip: ref.Tip, Branch: ref.Branch}
		}
		if holders, err := c.LeaseList(); err == nil {
			s.Leases = LeasePanel{Available: true, Holders: holders}
		}
		if integ, err := c.IntegrateList(); err == nil {
			s.Integrations = IntegrationPanel{Available: true, Integrations: integ}
		}
		if recs, err := c.IntegrationList(); err == nil {
			s.Reviews = ReviewPanel{Available: true, Records: recs}
		}
		if entries, err := c.AuditTail(auditLimit); err == nil {
			s.Audit = AuditPanel{Available: true, Entries: entries}
		}
		return s
	}
}
