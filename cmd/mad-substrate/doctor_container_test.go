package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func okImageRun(name string, args ...string) (string, error) {
	if len(args) >= 1 && args[0] == "image" {
		return "NAME    TAG     DIGEST\nalpine  latest  28bd5fe8b56d\n", nil
	}
	return "", nil // `container list -a -q`
}

func TestCheckContainerRuntimeAllOK(t *testing.T) {
	dir := t.TempDir()
	relay := filepath.Join(dir, "relay")
	if err := os.WriteFile(relay, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	look := func(string) (string, error) { return "/usr/local/bin/container", nil }
	p := checkContainerRuntime(&buf, true, "alpine:latest", relay, false, look, okImageRun, os.Stat)
	if p != 0 {
		t.Fatalf("expected 0 problems, got %d\n%s", p, buf.String())
	}
	s := buf.String()
	for _, want := range []string{"apiserver reachable", "cached", "relay"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestCheckContainerRuntimeCLIMissing(t *testing.T) {
	look := func(string) (string, error) { return "", errors.New("not found") }
	run := func(string, ...string) (string, error) { return "", nil }

	var buf bytes.Buffer
	if p := checkContainerRuntime(&buf, true, "alpine:latest", "", false, look, run, os.Stat); p != 1 {
		t.Fatalf("hardFail + CLI missing: expected 1 problem, got %d", p)
	}
	buf.Reset()
	if p := checkContainerRuntime(&buf, false, "alpine:latest", "", false, look, run, os.Stat); p != 0 {
		t.Fatalf("non-hardFail + CLI missing: expected 0 problems, got %d", p)
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Error("non-hardFail CLI-missing should WARN")
	}
}

func TestCheckContainerRuntimeApiserverDown(t *testing.T) {
	look := func(string) (string, error) { return "/c", nil }
	run := func(name string, args ...string) (string, error) { return "down", errors.New("conn refused") }
	var buf bytes.Buffer
	if p := checkContainerRuntime(&buf, true, "alpine:latest", "", false, look, run, os.Stat); p != 1 {
		t.Fatalf("apiserver down (hardFail): expected 1, got %d\n%s", p, buf.String())
	}
}

func TestCheckContainerRuntimeImageMissingIsWarn(t *testing.T) {
	look := func(string) (string, error) { return "/c", nil }
	run := func(name string, args ...string) (string, error) {
		if len(args) >= 1 && args[0] == "image" {
			return "NAME TAG DIGEST\n", nil // no images
		}
		return "", nil
	}
	var buf bytes.Buffer
	if p := checkContainerRuntime(&buf, true, "alpine:latest", "", false, look, run, os.Stat); p != 0 {
		t.Fatalf("image missing must be WARN not FAIL: got %d", p)
	}
	if !strings.Contains(buf.String(), "not cached") {
		t.Error("expected 'not cached' WARN")
	}
}

func TestCheckContainerRuntimeRelayMissingIsWarn(t *testing.T) {
	look := func(string) (string, error) { return "/c", nil }
	var buf bytes.Buffer
	if p := checkContainerRuntime(&buf, true, "alpine:latest", "/no/such/relay", false, look, okImageRun, os.Stat); p != 0 {
		t.Fatalf("relay missing must be WARN not FAIL: got %d", p)
	}
	if !strings.Contains(buf.String(), "cooperative plane will be off") {
		t.Error("expected relay-missing WARN")
	}
}

// TestCheckContainerRuntimeEmbeddedReported proves the embed-status line flips with
// coopembed.Available(): embedded -> the "embedded" report, not-embedded -> the
// build-without-coopembed WARNING. Reporting embed status is fail-soft (never a
// FAILURE), so problems must stay 0 in both directions.
func TestCheckContainerRuntimeEmbeddedReported(t *testing.T) {
	look := func(string) (string, error) { return "/c", nil }

	// embedded=true: positive report, no build WARN.
	var on bytes.Buffer
	if p := checkContainerRuntime(&on, true, "alpine:latest", "", true, look, okImageRun, os.Stat); p != 0 {
		t.Fatalf("embedded report must not FAIL: got %d", p)
	}
	if !strings.Contains(on.String(), "cooperative relay: embedded (container grain has the in-container cooperative plane)") {
		t.Errorf("expected embedded report, got:\n%s", on.String())
	}
	if strings.Contains(on.String(), "without -tags coopembed") {
		t.Errorf("embedded build must not emit the not-embedded WARN:\n%s", on.String())
	}

	// NON-VACUOUS CONTROL: embedded=false flips it to the WARNING (same inputs).
	var off bytes.Buffer
	if p := checkContainerRuntime(&off, true, "alpine:latest", "", false, look, okImageRun, os.Stat); p != 0 {
		t.Fatalf("not-embedded WARN is fail-soft, must not FAIL: got %d", p)
	}
	if !strings.Contains(off.String(), "WARN: this binary was built without -tags coopembed") {
		t.Errorf("expected not-embedded WARN, got:\n%s", off.String())
	}
	if strings.Contains(off.String(), "cooperative relay: embedded") {
		t.Errorf("not-embedded build must not claim an embedded relay:\n%s", off.String())
	}

	// When an explicit host relay overrides, the not-embedded WARN is suppressed
	// (the override IS the plane) — no embedded line and no build WARN.
	var override bytes.Buffer
	if p := checkContainerRuntime(&override, true, "alpine:latest", "/no/such/relay", false, look, okImageRun, os.Stat); p != 0 {
		t.Fatalf("override path must not FAIL: got %d", p)
	}
	if strings.Contains(override.String(), "without -tags coopembed") {
		t.Errorf("an explicit relay override should suppress the not-embedded WARN:\n%s", override.String())
	}
}
