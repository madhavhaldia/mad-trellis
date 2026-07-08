package launcher

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// inject.go is the PTY line-injection primitive the nudge plane delivers through.
//
// THE BUG IT FIXES: delivery used to be ONE PTY write of "body\r". Whether that
// trailing \r means "Enter keypress -> submit" or "newline inside pasted text" is
// decided by the CHILD TUI's input heuristics, not by us. Ink-based TUIs (Claude
// Code) submit on \r even mid-burst, so the single write happened to work there;
// burst-detecting TUIs (Codex) classify bytes arriving within a few milliseconds
// of each other as a PASTE, and an Enter inside a paste burst is inserted into
// the composer as a newline instead of submitting — the nudge landed in the chat
// box and sat there. Delivery correctness depended on WHICH agent sat behind the
// PTY, which is exactly the coupling Inv 10 forbids (the agent is swappable).
//
// THE FIX — make the two intents mechanically distinct for ANY input stack:
//   - The BODY is an "insert text" event: written as its own PTY write, wrapped
//     in bracketed-paste markers (ESC[200~ .. ESC[201~) whenever the child has
//     ENABLED bracketed paste (tracked from the child's own output — the mode-set
//     request ESC[?2004h / reset ESC[?2004l passes through the PTY relay we
//     already own). A bracketed body is unambiguously a paste: no premature
//     submit if the kernel chunks the write, no burst-window guessing.
//   - The SUBMIT is an "Enter keypress" event: a lone \r in its OWN write,
//     temporally isolated by submitDelay — comfortably above any paste-burst
//     detection window — so every TUI sees a deliberate keystroke.
//
// The injector holds the shared PTY writer mutex across the WHOLE body/delay/
// submit sequence, so the user's stdin relay can never interleave keystrokes
// between an injected body and its Enter. The hold is bounded by submitDelay
// (imperceptible) and delivery stays fail-soft: an injection error is returned
// to the nudge loop, which simply does not audit the delivery.

// defaultNudgeSubmitDelay isolates the submit \r from the body write. TUI paste-
// burst detectors classify on inter-byte gaps of a few milliseconds; 250ms is far
// outside any such window while staying imperceptible in the terminal.
const defaultNudgeSubmitDelay = 250 * time.Millisecond

// Bracketed-paste wire bytes: the mode set/reset the CHILD emits to request the
// mode (tracked by pasteModeTracker), and the markers WE emit around an injected
// body when the mode is on.
var (
	pasteModeSet   = []byte("\x1b[?2004h")
	pasteModeReset = []byte("\x1b[?2004l")
	pasteBegin     = []byte("\x1b[200~")
	pasteEnd       = []byte("\x1b[201~")
)

// lineInjector is the delivery seam the nudge loop writes through: inject one
// body as pasted text, then submit it as a deliberate Enter keypress.
type lineInjector interface {
	Inject(body string) error
}

// pasteModeTracker watches the CHILD's output stream for the bracketed-paste
// mode set/reset sequences, so the injector knows whether the child currently
// wants pastes bracketed. It sits on the output relay as a TeeReader sink: Write
// never fails and never alters the relayed bytes. A tail of the previous chunk
// is carried so a sequence split across reads is still recognized.
type pasteModeTracker struct {
	on   atomic.Bool
	mu   sync.Mutex
	tail []byte
}

// enabled reports whether the child has bracketed paste on right now.
func (t *pasteModeTracker) enabled() bool { return t.on.Load() }

// Write scans one relayed output chunk. Always succeeds (the relay must never
// stall on the tracker).
func (t *pasteModeTracker) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	buf := make([]byte, 0, len(t.tail)+len(p))
	buf = append(buf, t.tail...)
	buf = append(buf, p...)
	// Later occurrences win: scan forward, last mode change observed sticks.
	for i := 0; i+len(pasteModeSet) <= len(buf); i++ {
		if bytes.HasPrefix(buf[i:], pasteModeSet) {
			t.on.Store(true)
		} else if bytes.HasPrefix(buf[i:], pasteModeReset) {
			t.on.Store(false)
		}
	}
	// Carry enough tail to complete a sequence split at the chunk boundary.
	keep := len(pasteModeSet) - 1
	if len(buf) < keep {
		keep = len(buf)
	}
	t.tail = append(t.tail[:0], buf[len(buf)-keep:]...)
	return len(p), nil
}

// ptyInjector delivers injected lines into the child PTY as body-then-submit.
// mu is the SAME mutex the stdin relay writes under, held across the whole
// sequence so user keystrokes cannot interleave.
type ptyInjector struct {
	mu    *sync.Mutex
	w     io.Writer
	paste *pasteModeTracker   // nil -> never bracket
	delay time.Duration       // submit isolation delay (0 -> default)
	sleep func(time.Duration) // injectable for deterministic tests; nil -> time.Sleep
}

// Inject writes body as an insert-text event, waits out the paste-burst window,
// then writes a lone \r as a deliberate Enter keypress.
func (i *ptyInjector) Inject(body string) error {
	sleep := i.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	payload := []byte(body)
	if i.paste != nil && i.paste.enabled() {
		wrapped := make([]byte, 0, len(pasteBegin)+len(payload)+len(pasteEnd))
		wrapped = append(wrapped, pasteBegin...)
		wrapped = append(wrapped, payload...)
		wrapped = append(wrapped, pasteEnd...)
		payload = wrapped
	}
	if _, err := i.w.Write(payload); err != nil {
		return err
	}
	sleep(durationOr(i.delay, defaultNudgeSubmitDelay))
	_, err := i.w.Write([]byte("\r"))
	return err
}
