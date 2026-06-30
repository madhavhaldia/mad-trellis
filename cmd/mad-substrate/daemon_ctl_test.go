package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// daemonCmd must wire the two control subcommands (status, stop) AND keep its own
// RunE so the bare `mad-substrate daemon` still starts the daemon.
func TestDaemonCmdHasControlSubcommands(t *testing.T) {
	d := daemonCmd()
	if d.RunE == nil {
		t.Fatal("daemon command lost its RunE — bare `mad-substrate daemon` must still start the daemon")
	}
	want := map[string]bool{"status": false, "stop": false}
	for _, sub := range d.Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("daemon subcommand %q not wired", name)
		}
	}
}

// --socket must be a PERSISTENT flag on daemon so the subcommands inherit it.
func TestDaemonSocketFlagIsPersistent(t *testing.T) {
	d := daemonCmd()
	if d.PersistentFlags().Lookup("socket") == nil {
		t.Fatal("daemon --socket must be a persistent flag (so stop/status inherit it)")
	}
}

// `daemon status` against a dead socket must print "not running" and exit non-zero
// (signalled via errSilentExit so main() exits 1 without a noisy diagnostic).
func TestDaemonStatusDeadSocket(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "nope.sock")
	cmd := daemonStatusCmd(&dead)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("status against a dead socket must return a non-nil (non-zero exit) error")
	}
	se, ok := err.(errSilentExit)
	if !ok || se.code == 0 {
		t.Fatalf("expected errSilentExit with non-zero code, got %T %v", err, err)
	}
	if !strings.Contains(out.String(), "not running") {
		t.Fatalf("expected 'not running' message, got %q", out.String())
	}
	if !strings.Contains(out.String(), dead) {
		t.Fatalf("status message should name the socket %q, got %q", dead, out.String())
	}
}

// `daemon stop` is idempotent: stopping with no daemon reachable succeeds (exit 0)
// and reports nothing to stop.
func TestDaemonStopDeadSocketIdempotent(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "nope.sock")
	cmd := daemonStopCmd(&dead)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stop against a dead socket must be a clean no-op, got err %v", err)
	}
	if !strings.Contains(out.String(), "nothing to stop") {
		t.Fatalf("expected 'nothing to stop', got %q", out.String())
	}
}
