package launcher

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunPTYIONudgeInjectsBranchVerdict(t *testing.T) {
	var out bytes.Buffer
	var calls atomic.Int32
	source := func(context.Context) ([]nudgeEvent, error) {
		if calls.Add(1) == 1 {
			return []nudgeEvent{{Kind: "integration.verdict", Branch: "feature/nudge"}}, nil
		}
		return nil, nil
	}

	code, err := runPTYIOWithOptions(
		strings.NewReader(""), &out,
		ExecTarget{Cwd: t.TempDir()}, nil,
		"sh", []string{"-c", `IFS= read -r line; printf '%s' "$line"`},
		ptyRunOptions{Nudges: nudgeConfig{
			Audience:     "branch:feature/nudge",
			Branch:       "feature/nudge",
			Source:       source,
			PollInterval: 10 * time.Millisecond,
			QuietPeriod:  20 * time.Millisecond,
			SubmitDelay:  20 * time.Millisecond,
		}},
	)
	if err != nil {
		t.Fatalf("runPTYIOWithOptions: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	want := "[mad-trellis] your integration request on feature/nudge has a verdict — run mad_integration_status."
	if !strings.Contains(out.String(), want) {
		t.Fatalf("child input/output missing nudge line:\n got %q\nwant %q", out.String(), want)
	}
}

func TestRunPTYIONudgePolitenessDefersWhileUserInputIsFresh(t *testing.T) {
	var out bytes.Buffer
	var calls atomic.Int32
	source := func(context.Context) ([]nudgeEvent, error) {
		if calls.Add(1) == 1 {
			return []nudgeEvent{{Kind: "integration.claimed", Branch: "feature/polite"}}, nil
		}
		return nil, nil
	}

	start := time.Now()
	code, err := runPTYIOWithOptions(
		strings.NewReader("typed by user\n"), &out,
		ExecTarget{Cwd: t.TempDir()}, nil,
		"sh", []string{"-c", `IFS= read -r first; IFS= read -r second; printf '%s|%s' "$first" "$second"`},
		ptyRunOptions{Nudges: nudgeConfig{
			Audience:     "branch:feature/polite",
			Branch:       "feature/polite",
			Source:       source,
			PollInterval: 10 * time.Millisecond,
			QuietPeriod:  180 * time.Millisecond,
			RetryAfter:   15 * time.Millisecond,
			SubmitDelay:  20 * time.Millisecond,
		}},
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runPTYIOWithOptions: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if elapsed < 160*time.Millisecond {
		t.Fatalf("nudge delivered too soon after user input: elapsed=%s", elapsed)
	}
	if !strings.Contains(out.String(), "typed by user") {
		t.Fatalf("child did not receive user input: %q", out.String())
	}
	want := "[mad-trellis] your integration request on feature/polite was claimed for review."
	if !strings.Contains(out.String(), want) {
		t.Fatalf("child did not receive deferred nudge:\n got %q\nwant %q", out.String(), want)
	}
}

func TestRunPTYIONudgeDisabledByEnv(t *testing.T) {
	t.Setenv("MAD_NUDGES", "off")

	var out bytes.Buffer
	var calls atomic.Int32
	source := func(context.Context) ([]nudgeEvent, error) {
		calls.Add(1)
		return []nudgeEvent{{Kind: "integration.verdict", Branch: "feature/off"}}, nil
	}

	code, err := runPTYIOWithOptions(
		strings.NewReader(""), &out,
		ExecTarget{Cwd: t.TempDir()}, nil,
		"sh", []string{"-c", "sleep 0.1; exit 0"},
		ptyRunOptions{Nudges: nudgeConfig{
			Audience:     "branch:feature/off",
			Branch:       "feature/off",
			Source:       source,
			PollInterval: 10 * time.Millisecond,
			QuietPeriod:  10 * time.Millisecond,
		}},
	)
	if err != nil {
		t.Fatalf("runPTYIOWithOptions: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if calls.Load() != 0 {
		t.Fatalf("MAD_NUDGES=off must not poll the source; calls=%d", calls.Load())
	}
}

func TestRunPTYIONudgePollErrorIsFailSoft(t *testing.T) {
	var out bytes.Buffer
	var calls atomic.Int32
	source := func(context.Context) ([]nudgeEvent, error) {
		calls.Add(1)
		return nil, errors.New("daemon unavailable")
	}

	code, err := runPTYIOWithOptions(
		strings.NewReader(""), &out,
		ExecTarget{Cwd: t.TempDir()}, nil,
		"sh", []string{"-c", "sleep 0.1; printf ok"},
		ptyRunOptions{Nudges: nudgeConfig{
			Audience:     "branch:feature/error",
			Branch:       "feature/error",
			Source:       source,
			PollInterval: 10 * time.Millisecond,
			QuietPeriod:  10 * time.Millisecond,
		}},
	)
	if err != nil {
		t.Fatalf("runPTYIOWithOptions: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if calls.Load() == 0 {
		t.Fatal("source was never polled; test would be vacuous")
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("session did not continue after poll errors; output=%q", out.String())
	}
}

func TestRunPTYIOWithIdleNudgesPreservesExactExitCode(t *testing.T) {
	var out bytes.Buffer
	source := func(context.Context) ([]nudgeEvent, error) { return nil, nil }

	code, err := runPTYIOWithOptions(
		strings.NewReader(""), &out,
		ExecTarget{Cwd: t.TempDir()}, nil,
		"sh", []string{"-c", "exit 7"},
		ptyRunOptions{Nudges: nudgeConfig{
			Audience:     "branch:feature/idle",
			Branch:       "feature/idle",
			Source:       source,
			PollInterval: 10 * time.Millisecond,
			QuietPeriod:  10 * time.Millisecond,
		}},
	)
	if err != nil {
		t.Fatalf("runPTYIOWithOptions: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want exact child code 7", code)
	}
}
