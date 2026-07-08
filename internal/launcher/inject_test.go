package launcher

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// codexLikeComposer models the burst-detecting input stack of a Codex-style TUI:
// bytes arriving in one burst (a single write, or writes with no meaningful time
// gap) are treated as a PASTE, so a \r inside a burst inserts a newline into the
// composer instead of submitting. Only a LONE \r, temporally isolated from the
// preceding input by at least burstWindow, is a deliberate Enter keypress and
// submits the composed line. Simulated time advances ONLY via the injector's
// sleep hook, so the model is fully deterministic (no wall-clock in the test).
type codexLikeComposer struct {
	burstWindow time.Duration
	gap         time.Duration // simulated time since the previous write
	line        strings.Builder
	submitted   []string
}

func (c *codexLikeComposer) sleep(d time.Duration) { c.gap += d }

func (c *codexLikeComposer) Write(p []byte) (int, error) {
	isolatedEnter := len(p) == 1 && p[0] == '\r' && c.gap >= c.burstWindow
	c.gap = 0
	if isolatedEnter {
		c.submitted = append(c.submitted, c.line.String())
		c.line.Reset()
		return len(p), nil
	}
	for _, b := range p {
		if b == '\r' {
			c.line.WriteByte('\n') // Enter inside a burst = pasted newline
		} else {
			c.line.WriteByte(b)
		}
	}
	return len(p), nil
}

// TestInjectSubmitsAgainstBurstHeuristicComposer proves the body/delay/submit
// sequence registers as a real submission on a burst-detecting input stack —
// the Codex-shaped failure the injector exists to fix.
func TestInjectSubmitsAgainstBurstHeuristicComposer(t *testing.T) {
	comp := &codexLikeComposer{burstWindow: 20 * time.Millisecond}
	var mu sync.Mutex
	inj := &ptyInjector{mu: &mu, w: comp, delay: defaultNudgeSubmitDelay, sleep: comp.sleep}

	if err := inj.Inject("[mad-trellis] nudge line"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(comp.submitted) != 1 || comp.submitted[0] != "[mad-trellis] nudge line" {
		t.Fatalf("composer did not submit the injected body: submitted=%q composer=%q",
			comp.submitted, comp.line.String())
	}
}

// TestInjectControlSingleWriteDoesNotSubmit is the NON-VACUOUS CONTROL: the old
// delivery shape — body and \r in ONE write — must FAIL against the same
// composer model (text stranded in the chat box, nothing submitted). If this
// passes-by-submitting, the composer model distinguishes nothing and the test
// above is vacuous.
func TestInjectControlSingleWriteDoesNotSubmit(t *testing.T) {
	comp := &codexLikeComposer{burstWindow: 20 * time.Millisecond}
	if _, err := comp.Write([]byte("[mad-trellis] nudge line\r")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(comp.submitted) != 0 {
		t.Fatalf("control failed: single-write body+\\r submitted %q — the composer model is vacuous", comp.submitted)
	}
	if !strings.Contains(comp.line.String(), "[mad-trellis] nudge line") {
		t.Fatalf("control failed: body did not land in the composer: %q", comp.line.String())
	}
}

// TestInjectControlZeroDelayDoesNotSubmit proves the ISOLATION DELAY is the
// load-bearing part: the same two-write sequence with no time gap is still one
// burst, so the \r must NOT submit.
func TestInjectControlZeroDelayDoesNotSubmit(t *testing.T) {
	comp := &codexLikeComposer{burstWindow: 20 * time.Millisecond}
	var mu sync.Mutex
	inj := &ptyInjector{mu: &mu, w: comp, delay: defaultNudgeSubmitDelay, sleep: func(time.Duration) {}}

	if err := inj.Inject("body"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(comp.submitted) != 0 {
		t.Fatalf("control failed: un-isolated \\r submitted %q — the delay is not what makes submission register", comp.submitted)
	}
}

// recordingWriter captures each Write call separately, so a test can assert on
// write BOUNDARIES (one write for the body, one for the submit), not just bytes.
type recordingWriter struct{ writes [][]byte }

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

// TestInjectBracketsBodyWhenChildEnabledBracketedPaste: once the tracker has
// seen the child ENABLE bracketed paste (even split across output chunks), the
// injected body is wrapped in ESC[200~..ESC[201~ — explicitly a paste — and the
// submit stays a separate lone \r. After the child DISABLES the mode, the body
// goes plain again.
func TestInjectBracketsBodyWhenChildEnabledBracketedPaste(t *testing.T) {
	tracker := &pasteModeTracker{}
	// Mode-set sequence split across two relayed chunks.
	_, _ = tracker.Write([]byte("startup noise \x1b[?20"))
	_, _ = tracker.Write([]byte("04h more output"))
	if !tracker.enabled() {
		t.Fatal("tracker missed a mode-set sequence split across chunks")
	}

	rec := &recordingWriter{}
	var mu sync.Mutex
	inj := &ptyInjector{mu: &mu, w: rec, paste: tracker, delay: time.Millisecond, sleep: func(time.Duration) {}}
	if err := inj.Inject("hello"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(rec.writes) != 2 {
		t.Fatalf("want 2 writes (body, submit), got %d: %q", len(rec.writes), rec.writes)
	}
	if want := "\x1b[200~hello\x1b[201~"; string(rec.writes[0]) != want {
		t.Fatalf("body not bracketed: got %q want %q", rec.writes[0], want)
	}
	if string(rec.writes[1]) != "\r" {
		t.Fatalf("submit is not a lone \\r: %q", rec.writes[1])
	}

	// Child turns the mode off -> plain body.
	_, _ = tracker.Write([]byte("\x1b[?2004l"))
	if tracker.enabled() {
		t.Fatal("tracker missed the mode-reset sequence")
	}
	rec2 := &recordingWriter{}
	inj2 := &ptyInjector{mu: &mu, w: rec2, paste: tracker, delay: time.Millisecond, sleep: func(time.Duration) {}}
	if err := inj2.Inject("hello"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if string(rec2.writes[0]) != "hello" {
		t.Fatalf("body should be plain when bracketed paste is off: %q", rec2.writes[0])
	}
}

// TestPasteModeTrackerLastOccurrenceWins: a chunk carrying both set and reset
// lands on the LATER one, and the relayed byte count is always reported intact
// (the tracker must never stall the output relay).
func TestPasteModeTrackerLastOccurrenceWins(t *testing.T) {
	tracker := &pasteModeTracker{}
	chunk := []byte("\x1b[?2004h middle \x1b[?2004l")
	n, err := tracker.Write(chunk)
	if err != nil || n != len(chunk) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(chunk))
	}
	if tracker.enabled() {
		t.Fatal("reset after set in one chunk must leave the mode OFF")
	}
	_, _ = tracker.Write(bytes.Repeat([]byte("x"), 64)) // unrelated output keeps state
	_, _ = tracker.Write([]byte("\x1b[?2004h"))
	if !tracker.enabled() {
		t.Fatal("set after reset must leave the mode ON")
	}
}

// TestInjectHoldsWriterLockAcrossSequence: the injector must hold the shared PTY
// mutex across body+delay+submit so the stdin relay cannot interleave user
// keystrokes between an injected body and its Enter.
func TestInjectHoldsWriterLockAcrossSequence(t *testing.T) {
	rec := &recordingWriter{}
	var mu sync.Mutex
	interleaved := make(chan bool, 1)
	inj := &ptyInjector{mu: &mu, w: rec, delay: time.Millisecond, sleep: func(time.Duration) {
		// Mid-sequence (between body and submit): a concurrent writer taking the
		// same lock must NOT get in.
		interleaved <- mu.TryLock()
	}}
	if err := inj.Inject("body"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if <-interleaved {
		t.Fatal("PTY writer lock was acquirable between body and submit — user input could interleave")
	}
}
