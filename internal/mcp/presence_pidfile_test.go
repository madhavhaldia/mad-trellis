package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/coopclient"
	"github.com/madhavhaldia/mad-trellis/internal/runtimecfg"
)

// TestPresencePidfileWriteRemove is the NON-VACUOUS check for the pidfile
// primitive `integrator stop` depends on: writePresencePidfile lands a file at the
// runtimecfg-derived path containing THIS process's pid, and removePresencePidfile
// deletes it and clears the recorded path.
func TestPresencePidfileWriteRemove(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAD_RUNTIME_DIR", dir)

	s := &server{logf: func(string, ...any) {}}
	s.writePresencePidfile(integratorPresenceKey)

	raw, _ := base64.StdEncoding.DecodeString(integratorPresenceKey)
	path := runtimecfg.IntegratorPidfile(runtimecfg.SocketPath(""), raw)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("pidfile not written at %s: %v", path, err)
	}
	if got := strings.TrimSpace(string(b)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pidfile must hold our pid %d, got %q", os.Getpid(), got)
	}
	if s.presencePidfile != path {
		t.Fatalf("server must record the pidfile path, got %q", s.presencePidfile)
	}

	s.removePresencePidfile()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("pidfile must be removed, stat err=%v", err)
	}
	if s.presencePidfile != "" {
		t.Fatalf("recorded path must be cleared after removal, got %q", s.presencePidfile)
	}
}

// TestIntegratorServeRemovesPidfileOnShutdown proves the lifecycle is wired: a
// GRANTED integrator serve writes a presence pidfile, and a clean shutdown (stdin
// EOF here) removes it — so no pidfile is left behind to make `integrator stop`
// signal a pid this process no longer owns.
func TestIntegratorServeRemovesPidfileOnShutdown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAD_RUNTIME_DIR", dir)

	be := &stubBackend{acquireGranted: true, renewOK: true, releaseOK: true}
	cfg := coopclient.Config{LeaseTTL: 2 * time.Second, Session: "sess-1"}
	dial := func(coopclient.Config) (backend, error) { return be, nil }
	var out bytes.Buffer
	in := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := serveWith(ctx, strings.NewReader(in), &out, "v9", "integrator", func(string, ...any) {}, cfg, dial); err != nil {
		t.Fatalf("serveWith: %v", err)
	}

	// The granted branch ran (acquireGranted:true), so a pidfile was written during
	// serve; a clean shutdown must have removed it.
	matches, _ := filepath.Glob(filepath.Join(dir, "presence-*.pid"))
	if len(matches) != 0 {
		t.Fatalf("presence pidfile leaked after clean shutdown: %v", matches)
	}
}
