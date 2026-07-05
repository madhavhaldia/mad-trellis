package coophook

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// poisonReader fails the test if Run ever reads from it; the surviving hook
// (SessionStart) and the unknown-event path must NOT touch stdin.
type poisonReader struct{ t *testing.T }

func (p poisonReader) Read([]byte) (int, error) {
	p.t.Fatal("hook must not read stdin")
	return 0, io.EOF
}

// TestRunClaudeSessionStart asserts the SessionStart hook emits the
// additionalContext standing guidance and returns 0.
func TestRunClaudeSessionStart(t *testing.T) {
	var out, errw bytes.Buffer
	code := Run("claude-sessionstart", poisonReader{t: t}, &out, &errw)
	if code != 0 {
		t.Fatalf("code must be 0, got %d", code)
	}
	if errw.Len() != 0 {
		t.Fatalf("session-start must write nothing to stderr: %q", errw.String())
	}
	m := decodeObj(t, out.Bytes())
	hso, ok := m["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput: %s", out.String())
	}
	if hso["hookEventName"] != "SessionStart" {
		t.Fatalf("bad hookEventName: %s", out.String())
	}
	ac, _ := hso["additionalContext"].(string)
	if ac != sessionStartGuidance {
		t.Fatalf("additionalContext must be the standing guidance, got %q", ac)
	}
	if !strings.Contains(ac, "mad-trellis MCP tools") {
		t.Fatalf("guidance should reference the MCP tools: %q", ac)
	}
}

// TestRunUnknownEvent asserts an unknown/removed event (including the removed
// per-edit and lifecycle events) writes nothing and returns 0 without reading
// stdin.
func TestRunUnknownEvent(t *testing.T) {
	for _, ev := range []string{
		"nope",
		"claude-pretooluse",
		"codex-pretooluse",
		"claude-sessionend",
		"codex-stop",
		"",
	} {
		t.Run(ev, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := Run(ev, poisonReader{t: t}, &out, &errw)
			if code != 0 {
				t.Fatalf("%q must return 0, got %d", ev, code)
			}
			if out.Len() != 0 || errw.Len() != 0 {
				t.Fatalf("%q must write nothing, out=%q err=%q", ev, out.String(), errw.String())
			}
		})
	}
}

// TestRunAlwaysExitsZero pins the always-return-0 contract across the dispatch.
func TestRunAlwaysExitsZero(t *testing.T) {
	for _, ev := range []string{"claude-sessionstart", "nope", ""} {
		var out, errw bytes.Buffer
		if code := Run(ev, strings.NewReader(""), &out, &errw); code != 0 {
			t.Fatalf("event %q must exit 0, got %d", ev, code)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func decodeObj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("expected JSON, got empty")
	}
	// Compact: no trailing newline.
	if b[len(b)-1] == '\n' {
		t.Fatalf("output must be compact (no trailing newline): %q", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad JSON %q: %v", b, err)
	}
	return m
}
