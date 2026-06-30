package watch

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Lipgloss styles for the read-only surface. Colors are best-effort: a non-color
// terminal degrades to plain text (lipgloss handles the profile).
var (
	titleStyle        = lipgloss.NewStyle().Bold(true)
	focusedTitleStyle = lipgloss.NewStyle().Bold(true).Underline(true)
	dimStyle          = lipgloss.NewStyle().Faint(true)
	unavailableStyle  = lipgloss.NewStyle().Faint(true).Italic(true)
	headerStyle       = lipgloss.NewStyle().Bold(true)
)

// unavailableLine is the per-panel degraded message (a wedged/errored read).
const unavailableLine = "unavailable (daemon not reachable)"

// View renders the whole surface from the LATEST snapshot only. If the daemon is
// entirely unreachable it shows the friendly full-screen message (quit still
// works). It never panics on a missing/garbled field — every accessor degrades
// to a zero value or "unavailable".
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	// Full-screen unreachable path: only when we have a landed snapshot that says
	// the daemon could not be reached at all. Before the first poll we show a
	// brief connecting line rather than a false "cannot reach".
	if m.hasSnap && !m.snap.DaemonReachable {
		return m.renderUnreachable()
	}

	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")
	b.WriteString(m.renderPanel(panelTrunk, "Trunk state", m.trunkLines()))
	b.WriteString("\n\n")
	b.WriteString(m.renderPanel(panelPending, "Pending / in-flight integrations", m.pendingLines()))
	b.WriteString("\n\n")
	b.WriteString(m.renderPanel(panelReview, "Review queue", m.reviewQueueLines()))
	b.WriteString("\n\n")
	b.WriteString(m.renderPanel(panelLeases, "Lease holders", m.leaseLines()))
	b.WriteString("\n\n")
	b.WriteString(m.renderPanel(panelAudit, "Decision-audit stream", m.auditLines()))
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("read-only · q quit · tab/←→ focus · j/k scroll"))
	return b.String()
}

func (m Model) renderHeader() string {
	who := m.snap.Whoami
	if who == "" {
		who = "(session unavailable)"
	}
	if !m.hasSnap {
		return headerStyle.Render("mad-substrate watch") + dimStyle.Render("  — connecting…")
	}
	return headerStyle.Render("mad-substrate watch") +
		dimStyle.Render(fmt.Sprintf("  — read-only · watcher session %s", who))
}

func (m Model) renderUnreachable() string {
	sock := m.snap.Socket
	if sock == "" {
		sock = "(unknown socket)"
	}
	msg := fmt.Sprintf("cannot reach daemon at %s\n\nThe watch surface is non-load-bearing: nothing is wrong with governance.\nStart `mad-substrate daemon`, or press q to quit.", sock)
	return titleStyle.Render("mad-substrate watch") + "\n\n" + msg + "\n"
}

// renderPanel draws a titled panel; the focused panel's title is underlined so
// the user knows which one scrolls. Lines are scrolled by the panel's offset.
func (m Model) renderPanel(p panelID, title string, lines []string) string {
	ts := titleStyle
	marker := "  "
	if p == m.focus {
		ts = focusedTitleStyle
		marker = "▸ "
	}
	var b strings.Builder
	b.WriteString(marker + ts.Render(title))
	b.WriteString("\n")
	off := m.offsets[p]
	if off >= len(lines) {
		off = 0
	}
	for _, ln := range lines[off:] {
		b.WriteString("  " + ln + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- panel content (pure functions of the snapshot) -------------------------

// trunkLines renders the AUTHORITATIVE trunk tip — the git ref the integrator's
// CAS last advanced, read via integrate.trunk. It is NOT derived from the
// integrate.list ordering: promote order is not created_at order, so an
// out-of-order promote makes "last promoted in the list" the SUPERSEDED commit.
// We mirror the daemon's ref directly (Inv 12-readsurface: don't derive governed
// state locally) and only TALLY the promoted count from integrate.list.
func (m Model) trunkLines() []string {
	if !m.snap.Trunk.Available {
		return []string{unavailableStyle.Render(unavailableLine)}
	}
	if !m.snap.Trunk.Exists || m.snap.Trunk.Tip == "" {
		return []string{"trunk: (no integrations yet)"}
	}
	lines := []string{fmt.Sprintf("trunk tip: %s", short12(m.snap.Trunk.Tip))}
	if m.snap.Integrations.Available {
		promoted := 0
		for _, in := range m.snap.Integrations.Integrations {
			if in.State == "promoted" {
				promoted++
			}
		}
		lines = append(lines, dimStyle.Render(fmt.Sprintf("promoted integrations: %d", promoted)))
	}
	return lines
}

// pendingLines lists received/validating (in-flight) integrations, and surfaces
// aborted entries as display-only "conflicts/aborted". promoted entries are the
// trunk panel's job, not shown here.
func (m Model) pendingLines() []string {
	if !m.snap.Integrations.Available {
		return []string{unavailableStyle.Render(unavailableLine)}
	}
	var lines []string
	for _, in := range m.snap.Integrations.Integrations {
		switch in.State {
		case "received", "validating":
			lines = append(lines, fmt.Sprintf("%s  %-10s  branch %s  holder %s",
				in.ID, in.State, in.Branch, sessShort(in.Holder)))
		case "aborted":
			// Display-only: v1 carries no per-entry reason on integrate.list, so we
			// label it without fabricating a reason string.
			lines = append(lines, dimStyle.Render(fmt.Sprintf("%s  conflicts/aborted (display only)  branch %s",
				in.ID, in.Branch)))
		}
	}
	if len(lines) == 0 {
		return []string{dimStyle.Render("no pending or in-flight integrations")}
	}
	return lines
}

// reviewStateRank orders the Wing-3 REVIEW queue for display: open work first
// (requested, then claimed), then recent verdicts (changes_requested, approved),
// then withdrawn. An unknown state sorts last but is still shown verbatim.
func reviewStateRank(state string) int {
	switch state {
	case "requested":
		return 0
	case "claimed":
		return 1
	case "changes_requested":
		return 2
	case "approved":
		return 3
	case "withdrawn":
		return 4
	default:
		return 5
	}
}

// reviewQueueLines renders the Wing-3 integration REVIEW queue (integration.list):
// each record as "branch  [state by claimer]  "title"", with a truncated feedback
// note on changes_requested and the short merge OID on approved. Records are
// grouped by state (requested -> claimed -> verdicts -> withdrawn) and, within a
// group, newest-updated first. It is DISPLAY-ONLY: there is no claim/verdict
// affordance here (the watch surface mutates nothing).
func (m Model) reviewQueueLines() []string {
	if !m.snap.Reviews.Available {
		return []string{unavailableStyle.Render(unavailableLine)}
	}
	recs := m.snap.Reviews.Records
	if len(recs) == 0 {
		return []string{dimStyle.Render("no review requests")}
	}
	// Stable group/sort: state rank, then newest-updated first.
	sorted := make([]IntegrationRecord, len(recs))
	copy(sorted, recs)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := reviewStateRank(sorted[i].State), reviewStateRank(sorted[j].State)
		if ri != rj {
			return ri < rj
		}
		return sorted[i].UpdatedAtMs > sorted[j].UpdatedAtMs
	})
	lines := make([]string, 0, len(sorted))
	for _, r := range sorted {
		branch := r.Branch
		if branch == "" {
			branch = r.ID
		}
		// State tag, annotated with the claimer once a record is claimed/decided.
		tag := r.State
		switch r.State {
		case "claimed", "changes_requested", "approved":
			if r.Claimer != "" {
				tag = fmt.Sprintf("%s by %s", r.State, sessShort(r.Claimer))
			}
		}
		line := fmt.Sprintf("%-28s [%s]", branch, tag)
		if title := strings.TrimSpace(r.Title); title != "" {
			line += fmt.Sprintf("  %q", title)
		}
		// Verdict detail: feedback on changes_requested, merge OID on approved.
		switch r.State {
		case "changes_requested":
			if fb := strings.TrimSpace(r.Feedback); fb != "" {
				line += dimStyle.Render("  — " + truncate(fb, 60))
			}
		case "approved":
			if r.Merge != "" {
				line += dimStyle.Render("  — merged " + short12(r.Merge))
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// leaseLines renders each held lease as "held by session <h> — expires in <N>s".
// Waiters are LITERAL "none": v1 is CAS-fail-fast with NO queue, so we never
// fabricate a queue.
func (m Model) leaseLines() []string {
	if !m.snap.Leases.Available {
		return []string{unavailableStyle.Render(unavailableLine)}
	}
	lines := []string{"waiters: none (fail-fast, no queue)"}
	if len(m.snap.Leases.Holders) == 0 {
		lines = append(lines, dimStyle.Render("no leases held"))
		return lines
	}
	now := time.Now().UnixMilli()
	for _, h := range m.snap.Leases.Holders {
		lines = append(lines, fmt.Sprintf("held by session %s — expires in %s",
			sessShort(h.Holder), expiresIn(h.ExpiresAtMs, now)))
	}
	return lines
}

// auditLines renders newest-first decision-audit lines:
// "<project>/<kind> · session <s> · <relative-time>".
func (m Model) auditLines() []string {
	if !m.snap.Audit.Available {
		return []string{unavailableStyle.Render(unavailableLine)}
	}
	if len(m.snap.Audit.Entries) == 0 {
		return []string{dimStyle.Render("no decisions recorded yet")}
	}
	now := time.Now().UnixMilli()
	lines := make([]string, 0, len(m.snap.Audit.Entries))
	for _, e := range m.snap.Audit.Entries {
		lines = append(lines, fmt.Sprintf("%s/%s · session %s · %s",
			e.DecisionProject, e.DecisionKind, sessShort(e.Session), relativeTime(e.TimestampMs, now)))
	}
	return lines
}

// panelLineCounts returns the rendered line count per panel (for scroll
// clamping). It mirrors the per-panel content functions exactly.
func (m Model) panelLineCounts() [panelCount]int {
	return [panelCount]int{
		panelTrunk:   len(m.trunkLines()),
		panelPending: len(m.pendingLines()),
		panelReview:  len(m.reviewQueueLines()),
		panelLeases:  len(m.leaseLines()),
		panelAudit:   len(m.auditLines()),
	}
}

// --- formatting helpers ------------------------------------------------------

func short12(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	if oid == "" {
		return "∅"
	}
	return oid
}

// truncate clips s to at most max runes, appending an ellipsis when it had to
// cut. A non-positive max returns "".
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// sessShort trims the random suffix off a daemon-minted "s-N-<hex>" session for
// readability, keeping it unambiguous. A non-matching string is returned as-is.
func sessShort(s string) string {
	if s == "" {
		return "(none)"
	}
	parts := strings.SplitN(s, "-", 3)
	if len(parts) == 3 && parts[0] == "s" {
		hex := parts[2]
		if len(hex) > 6 {
			hex = hex[:6]
		}
		return "s-" + parts[1] + "-" + hex
	}
	return s
}

// expiresIn renders a human "Ns" until expiry; a past/zero expiry reads
// "expired".
func expiresIn(expiresAtMs, nowMs int64) string {
	if expiresAtMs <= 0 {
		return "unknown"
	}
	d := time.Duration(expiresAtMs-nowMs) * time.Millisecond
	if d <= 0 {
		return "expired"
	}
	return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
}

// relativeTime renders a coarse "Ns ago" / "Nm ago" / "Nh ago" for an audit
// timestamp. A zero/garbled timestamp reads "unknown".
func relativeTime(tsMs, nowMs int64) string {
	if tsMs <= 0 {
		return "unknown"
	}
	d := time.Duration(nowMs-tsMs) * time.Millisecond
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
