package watch

import (
	"encoding/json"
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
	alertStyle        = lipgloss.NewStyle().Bold(true)
)

const (
	unavailableLine    = "unavailable (daemon not reachable)"
	integratorLeaseKey = "mad-trellis:integrator:v1"
	footerLine         = "read-only · q quit · tab focus · j/k scroll · newest at bottom"
)

type layoutBudget struct {
	queueContent int
	feedContent  int
}

// View renders the whole surface from the LATEST snapshot only. If the daemon is
// entirely unreachable it shows the friendly full-screen message (quit still
// works). Once Bubble Tea has delivered a size, the returned string is capped to
// that height; scrolling happens inside the queue/feed regions.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	if !m.hasSnap {
		return m.capLines(m.renderConnecting())
	}
	if !m.snap.DaemonReachable {
		return m.capLines(m.renderUnreachable())
	}

	status := m.statusStripLines()
	queue := m.reviewQueueLines()
	feed := m.feedLines()
	budget := m.layoutBudget(status, queue, feed)

	lines := make([]string, 0, m.height)
	lines = append(lines, status...)
	lines = append(lines, m.regionTitle(panelQueue, "Review queue"))
	lines = append(lines, m.windowedRegion(panelQueue, queue, budget.queueContent, queueOverflowMarker)...)
	lines = append(lines, m.regionTitle(panelFeed, "Coordination feed"))
	lines = append(lines, m.windowedRegion(panelFeed, feed, budget.feedContent, feedOverflowMarker)...)
	lines = append(lines, dimStyle.Render(footerLine))
	return m.capLines(strings.Join(lines, "\n"))
}

func (m Model) renderConnecting() string {
	return headerStyle.Render("mad-trellis watch") + dimStyle.Render(" — connecting…")
}

func (m Model) renderUnreachable() string {
	sock := m.snap.Socket
	if sock == "" {
		sock = "(unknown socket)"
	}
	msg := fmt.Sprintf("cannot reach daemon at %s\n\nThe watch surface is non-load-bearing: nothing is wrong with governance.\nStart `mad-trellis daemon`, or press q to quit.", sock)
	return titleStyle.Render("mad-trellis watch") + "\n\n" + msg
}

func (m Model) statusStripLines() []string {
	who := m.snap.Whoami
	if who == "" {
		who = "(session unavailable)"
	}
	lines := []string{
		headerStyle.Render(fmt.Sprintf("mad-trellis watch — read-only · watcher session %s", sessShort(who))),
	}

	requested, claimed := m.openReviewCounts()
	reviewText := "reviews unavailable"
	if m.snap.Reviews.Available {
		reviewText = fmt.Sprintf("%d requested / %d claimed", requested, claimed)
	}

	integratorText := "integrator: unavailable"
	leaseText := "leases held: unavailable"
	present := false
	if m.snap.Leases.Available {
		holder := m.integratorHolder()
		if holder != "" {
			present = true
			integratorText = fmt.Sprintf("integrator: present (%s)", sessShort(holder))
		} else {
			integratorText = "integrator: ABSENT"
		}
		leaseText = fmt.Sprintf("leases held: %d", len(m.snap.Leases.Holders))
	}

	lines = append(lines, fmt.Sprintf("%s · %s · %s · %s", m.trunkStatus(), reviewText, integratorText, leaseText))
	if m.snap.Reviews.Available && m.snap.Leases.Available && requested > 0 && !present {
		lines = append(lines, alertStyle.Render(fmt.Sprintf("integrator: ABSENT — %d request(s) waiting · run: mad-trellis integrator start", requested)))
	}
	return lines
}

func (m Model) trunkStatus() string {
	if !m.snap.Trunk.Available {
		return "trunk " + unavailableLine
	}
	if !m.snap.Trunk.Exists || m.snap.Trunk.Tip == "" {
		return "trunk ∅"
	}
	return "trunk " + short12(m.snap.Trunk.Tip)
}

func (m Model) openReviewCounts() (requested, claimed int) {
	if !m.snap.Reviews.Available {
		return 0, 0
	}
	for _, r := range m.snap.Reviews.Records {
		switch r.State {
		case "requested":
			requested++
		case "claimed":
			claimed++
		}
	}
	return requested, claimed
}

func (m Model) integratorHolder() string {
	if !m.snap.Leases.Available {
		return ""
	}
	for _, h := range m.snap.Leases.Holders {
		if isIntegratorLeaseKey(h.Key) {
			return h.Holder
		}
	}
	return ""
}

func isIntegratorLeaseKey(key string) bool {
	return key == integratorLeaseKey || strings.HasPrefix(key, integratorLeaseKey+":slot-")
}

func (m Model) regionTitle(p panelID, title string) string {
	style := titleStyle
	marker := "  "
	if p == m.focus {
		style = focusedTitleStyle
		marker = "▸ "
	}
	return marker + style.Render(title)
}

func (m Model) layoutBudget(status, queue, feed []string) layoutBudget {
	if m.height <= 0 {
		return layoutBudget{queueContent: queueContentBudget(len(queue), 0), feedContent: len(feed)}
	}

	available := m.height - len(status) - 1 // footer
	if available < 0 {
		available = 0
	}

	queueNatural := 1 + queueContentBudget(len(queue), 0)
	minFeedHeight := 1
	queueHeight := queueNatural
	if maxQueue := available - minFeedHeight; queueHeight > maxQueue {
		queueHeight = maxQueue
	}
	if queueHeight < 0 {
		queueHeight = 0
	}
	feedHeight := available - queueHeight
	if feedHeight < 0 {
		feedHeight = 0
	}
	return layoutBudget{
		queueContent: queueContentBudget(len(queue), max(0, queueHeight-1)),
		feedContent:  max(0, feedHeight-1),
	}
}

func queueContentBudget(total, hardCap int) int {
	n := total
	if n > 6 {
		n = 7 // six visible lines plus an overflow marker
	}
	if hardCap > 0 && n > hardCap {
		n = hardCap
	}
	return n
}

func (m Model) windowedRegion(p panelID, lines []string, height int, marker func(int) string) []string {
	if height <= 0 {
		return nil
	}
	if len(lines) <= height {
		return lines
	}

	offset := m.offsets[p]
	maxOffset := maxScrollOffset(len(lines), height)
	if offset > maxOffset {
		offset = maxOffset
	}
	if offset < 0 {
		offset = 0
	}

	topMarker := offset > 0
	bottomMarker := false
	visibleBudget := height
	for {
		visibleBudget = height
		if topMarker {
			visibleBudget--
		}
		if bottomMarker {
			visibleBudget--
		}
		if visibleBudget < 0 {
			visibleBudget = 0
		}
		nextBottom := offset+visibleBudget < len(lines)
		if nextBottom == bottomMarker {
			break
		}
		bottomMarker = nextBottom
	}

	out := make([]string, 0, height)
	if topMarker {
		out = append(out, marker(offset))
	}
	end := offset + visibleBudget
	if end > len(lines) {
		end = len(lines)
	}
	out = append(out, lines[offset:end]...)
	if bottomMarker {
		out = append(out, marker(len(lines)-end))
	}
	return out
}

func queueOverflowMarker(hidden int) string {
	return dimStyle.Render(fmt.Sprintf("… +%d more (tab to focus, j/k)", hidden))
}

func feedOverflowMarker(hidden int) string {
	return dimStyle.Render(fmt.Sprintf("… +%d more", hidden))
}

func (m Model) capLines(rendered string) string {
	if m.height <= 0 || rendered == "" {
		return rendered
	}
	lines := strings.Split(rendered, "\n")
	if len(lines) <= m.height {
		return rendered
	}
	return strings.Join(lines[:m.height], "\n")
}

// reviewQueueLines renders ONLY open REVIEW work (requested/claimed). Verdicts
// and withdrawals are rendered in the feed.
func (m Model) reviewQueueLines() []string {
	if !m.snap.Reviews.Available {
		return []string{unavailableStyle.Render(unavailableLine)}
	}
	open := make([]IntegrationRecord, 0, len(m.snap.Reviews.Records))
	for _, r := range m.snap.Reviews.Records {
		if r.State == "requested" || r.State == "claimed" {
			open = append(open, r)
		}
	}
	if len(open) == 0 {
		return []string{dimStyle.Render("no open review requests")}
	}
	sort.SliceStable(open, func(i, j int) bool {
		ri, rj := reviewStateRank(open[i].State), reviewStateRank(open[j].State)
		if ri != rj {
			return ri < rj
		}
		return open[i].UpdatedAtMs > open[j].UpdatedAtMs
	})

	lines := make([]string, 0, len(open))
	for _, r := range open {
		branch := r.Branch
		if branch == "" {
			branch = r.ID
		}
		tag := r.State
		if r.State == "claimed" && r.Claimer != "" {
			tag = fmt.Sprintf("%s by %s", r.State, sessShort(r.Claimer))
		}
		line := fmt.Sprintf("%-28s [%s]", branch, tag)
		if title := strings.TrimSpace(r.Title); title != "" {
			line += fmt.Sprintf("  %q", title)
		}
		lines = append(lines, line)
	}
	return lines
}

func reviewStateRank(state string) int {
	switch state {
	case "requested":
		return 0
	case "claimed":
		return 1
	default:
		return 2
	}
}

type feedPayload struct {
	Branch   string `json:"branch"`
	Title    string `json:"title"`
	Claimer  string `json:"claimer"`
	Merge    string `json:"merge"`
	Feedback string `json:"feedback"`
	Holder   string `json:"holder"`
	Key      string `json:"key"`
	Audience string `json:"audience"`
	Kind     string `json:"kind"`
	Commit   string `json:"commit"`
}

type feedGroup struct {
	first AuditEntry
	last  AuditEntry
	count int
}

func (m Model) feedLines() []string {
	if !m.snap.Audit.Available {
		return []string{unavailableStyle.Render(unavailableLine)}
	}
	if len(m.snap.Audit.Entries) == 0 {
		return []string{dimStyle.Render("no decisions recorded yet")}
	}

	now := time.Now().UnixMilli()
	groups := dedupeAuditEntries(m.snap.Audit.Entries)
	lines := make([]string, 0, len(groups))
	for _, g := range groups {
		base, dim := feedBaseLine(g.first)
		line := base
		if g.count > 1 {
			line += fmt.Sprintf(" · ×%d · %s", g.count, relativeRange(g.first.TimestampMs, g.last.TimestampMs, now))
		} else {
			line += " · " + relativeTime(g.first.TimestampMs, now)
		}
		if dim {
			line = dimStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return lines
}

func dedupeAuditEntries(newestFirst []AuditEntry) []feedGroup {
	if len(newestFirst) == 0 {
		return nil
	}
	groups := make([]feedGroup, 0, len(newestFirst))
	for i := len(newestFirst) - 1; i >= 0; i-- {
		e := newestFirst[i]
		if len(groups) > 0 && sameFeedKey(groups[len(groups)-1].last, e) {
			groups[len(groups)-1].last = e
			groups[len(groups)-1].count++
			continue
		}
		groups = append(groups, feedGroup{first: e, last: e, count: 1})
	}
	return groups
}

func sameFeedKey(a, b AuditEntry) bool {
	return a.DecisionProject == b.DecisionProject &&
		a.DecisionKind == b.DecisionKind &&
		normalizePayload(a.Payload) == normalizePayload(b.Payload)
}

func normalizePayload(raw json.RawMessage) string {
	return strings.TrimSpace(string(raw))
}

func feedBaseLine(e AuditEntry) (string, bool) {
	payload, ok := parseFeedPayload(e.Payload)
	if !ok {
		return rawKindLine(e), true
	}

	switch {
	case e.DecisionProject == "integration-review" && e.DecisionKind == "integration.requested":
		return fmt.Sprintf("builder %s · requested %s · %q", sessShort(e.Session), payload.Branch, payload.Title), false
	case e.DecisionProject == "integration-review" && e.DecisionKind == "integration.claimed":
		claimer := payload.Claimer
		if claimer == "" {
			claimer = e.Session
		}
		return fmt.Sprintf("integrator %s · claimed %s", sessShort(claimer), payload.Branch), false
	case e.DecisionProject == "integration-review" && e.DecisionKind == "integration.approved":
		return fmt.Sprintf("integrator %s · approved %s — merged %s", sessShort(e.Session), payload.Branch, short12(payload.Merge)), false
	case e.DecisionProject == "integration-review" && e.DecisionKind == "integration.changes_requested":
		return fmt.Sprintf("integrator %s · changes requested on %s — %q", sessShort(e.Session), payload.Branch, truncate(payload.Feedback, 60)), false
	case e.DecisionProject == "integration-review" && e.DecisionKind == "integration.withdrawn":
		return fmt.Sprintf("builder %s · withdrew %s", sessShort(e.Session), payload.Branch), true
	case e.DecisionProject == "integration-review" && e.DecisionKind == "integration.requeued":
		return fmt.Sprintf("substrate · requeued %s (claimer died)", payload.Branch), true
	case e.DecisionProject == "integrator-trunk":
		target := payload.Branch
		if target == "" {
			target = short12(firstNonEmpty(payload.Commit, payload.Merge))
		}
		return fmt.Sprintf("integrator · %s %s", e.DecisionKind, target), true
	case e.DecisionKind == "nudge.delivered":
		return fmt.Sprintf("substrate · nudged %s", payload.Audience), true
	default:
		return rawAuditLine(e), true
	}
}

func parseFeedPayload(raw json.RawMessage) (feedPayload, bool) {
	var payload feedPayload
	if strings.TrimSpace(string(raw)) == "" {
		return payload, true
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return feedPayload{}, false
	}
	return payload, true
}

func rawAuditLine(e AuditEntry) string {
	return fmt.Sprintf("substrate · %s/%s · session %s", e.DecisionProject, e.DecisionKind, sessShort(e.Session))
}

func rawKindLine(e AuditEntry) string {
	return fmt.Sprintf("%s/%s · session %s", e.DecisionProject, e.DecisionKind, sessShort(e.Session))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (m Model) panelLineCounts() [panelCount]int {
	return [panelCount]int{
		panelFeed:  len(m.feedLines()),
		panelQueue: len(m.reviewQueueLines()),
		panelStrip: 0,
	}
}

func (m Model) panelContentBudgets() [panelCount]int {
	status := m.statusStripLines()
	queue := m.reviewQueueLines()
	feed := m.feedLines()
	budget := m.layoutBudget(status, queue, feed)
	return [panelCount]int{
		panelFeed:  budget.feedContent,
		panelQueue: budget.queueContent,
		panelStrip: 0,
	}
}

func maxScrollOffset(total, budget int) int {
	if total <= 0 || budget <= 0 || total <= budget {
		return 0
	}
	visible := budget - 1
	if visible < 1 {
		visible = 1
	}
	return total - visible
}

func short12(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	if oid == "" {
		return "∅"
	}
	return oid
}

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
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func relativeRange(oldestMs, newestMs, nowMs int64) string {
	oldest := strings.TrimSuffix(relativeTime(oldestMs, nowMs), " ago")
	newest := strings.TrimSuffix(relativeTime(newestMs, nowMs), " ago")
	if oldest == "unknown" || newest == "unknown" {
		return "unknown"
	}
	return oldest + "→" + newest + " ago"
}
