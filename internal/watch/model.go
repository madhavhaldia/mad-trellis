package watch

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// DefaultInterval is the poll cadence: a pure tea.Tick re-fetch. The daemon is
// the single source of truth; each tick replaces the whole Snapshot (no local
// derivation of governed state).
const DefaultInterval = 1500 * time.Millisecond

// DefaultAuditLimit bounds the audit tail the model requests.
const DefaultAuditLimit = 50

// panelID identifies the focusable panels (navigation only — focus changes
// nothing governed; it just chooses which panel scrolls).
type panelID int

const (
	panelTrunk panelID = iota
	panelPending
	panelReview
	panelLeases
	panelAudit
	panelCount // sentinel: number of panels
)

// snapshotMsg carries a freshly fetched Snapshot into Update.
type snapshotMsg struct{ snap Snapshot }

// tickMsg fires the next poll.
type tickMsg struct{}

// Model is the read-only watch TUI state (Elm architecture). It holds the LATEST
// snapshot, the focused panel, and per-panel scroll offsets. It never holds an
// action affordance: keybindings are STRICTLY navigational. The fetch func is
// injected so the model is testable without a live daemon.
type Model struct {
	fetch    Fetcher
	interval time.Duration

	snap    Snapshot
	hasSnap bool // false until the first poll lands (pre-first-fetch render)

	focus   panelID
	offsets [panelCount]int

	width, height int
	quitting      bool
}

// NewModel builds a watch Model. fetch is the Snapshot source (production wraps
// a read-only Client; tests inject a stub). interval<=0 uses DefaultInterval.
func NewModel(fetch Fetcher, interval time.Duration) Model {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return Model{fetch: fetch, interval: interval, focus: panelTrunk}
}

// Init kicks off the first fetch immediately, then schedules the poll loop.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetchCmd(), m.tickCmd())
}

// fetchCmd runs the injected fetch off the Update loop and delivers a
// snapshotMsg. Because fetch is bounded by the client's per-call deadline, this
// command cannot hang the program.
func (m Model) fetchCmd() tea.Cmd {
	fetch := m.fetch
	return func() tea.Msg {
		if fetch == nil {
			return snapshotMsg{}
		}
		return snapshotMsg{snap: fetch()}
	}
}

func (m Model) tickCmd() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}

// Update is the pure transition. KEYBINDINGS ARE STRICTLY NAVIGATIONAL: quit,
// move panel focus, scroll the focused panel. There is deliberately NO action
// key (no approve/retry/resolve/abort/promote/dispatch/submit) — the watch
// surface must offer no affordance that mutates or triggers anything.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case snapshotMsg:
		m.snap = msg.snap
		m.hasSnap = true
		m.clampOffsets()
		return m, nil

	case tickMsg:
		// Pure poll: fetch a fresh snapshot, then re-arm the tick.
		return m, tea.Batch(m.fetchCmd(), m.tickCmd())

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey processes ONLY navigational keys. Any other key is ignored.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit

	case "tab", "right", "l":
		m.focus = (m.focus + 1) % panelCount
		return m, nil
	case "shift+tab", "left", "h":
		m.focus = (m.focus - 1 + panelCount) % panelCount
		return m, nil

	case "down", "j":
		m.offsets[m.focus]++
		m.clampOffsets()
		return m, nil
	case "up", "k":
		if m.offsets[m.focus] > 0 {
			m.offsets[m.focus]--
		}
		return m, nil
	}
	// Unrecognized key: NO-OP. (No action affordance exists.)
	return m, nil
}

// clampOffsets keeps each scroll offset within its panel's line count so
// scrolling can never index past the data.
func (m *Model) clampOffsets() {
	lines := m.panelLineCounts()
	for p := panelID(0); p < panelCount; p++ {
		max := lines[p] - 1
		if max < 0 {
			max = 0
		}
		if m.offsets[p] > max {
			m.offsets[p] = max
		}
		if m.offsets[p] < 0 {
			m.offsets[p] = 0
		}
	}
}
