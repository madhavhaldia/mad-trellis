package coopclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-trellis/internal/protocol"
)

// clearEnv wipes every cooperative-client input so each Load case starts from a
// known floor. t.Setenv restores them after the test; we Unsetenv (not Setenv
// "") so an absent var is truly absent, since Load distinguishes "" from unset
// only by trimming — both must read as empty here.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MAD_SOCKET", "MAD_RUNTIME_DIR", "MAD_HOME",
		"MAD_SESSION", "MAD_SESSION_TOKEN",
		"MAD_LEASE_TTL_MS", "MAD_RPC_TIMEOUT_MS",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// NOTE: no t.Parallel — every case mutates process env via t.Setenv.

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	// Pin the runtime dir so Socket is deterministic and we don't touch the real
	// ~/.mad-trellis.
	rd := t.TempDir()
	t.Setenv("MAD_RUNTIME_DIR", rd)

	cfg := Load()
	if cfg.Session != "" {
		t.Errorf("Session: want empty, got %q", cfg.Session)
	}
	if cfg.Token != "" {
		t.Errorf("Token: want empty, got %q", cfg.Token)
	}
	if cfg.LeaseTTL != defaultLeaseTTL {
		t.Errorf("LeaseTTL: want %s, got %s", defaultLeaseTTL, cfg.LeaseTTL)
	}
	if cfg.RPCTimeout != defaultRPCTimeout {
		t.Errorf("RPCTimeout: want %s, got %s", defaultRPCTimeout, cfg.RPCTimeout)
	}
	wantSock := filepath.Join(rd, "daemon.sock")
	if cfg.Socket != wantSock {
		t.Errorf("Socket: want %q, got %q", wantSock, cfg.Socket)
	}
}

func TestLoadSocketEnvWins(t *testing.T) {
	clearEnv(t)
	t.Setenv("MAD_RUNTIME_DIR", t.TempDir())
	t.Setenv("MAD_SOCKET", "/explicit/daemon.sock")
	if got := Load().Socket; got != "/explicit/daemon.sock" {
		t.Errorf("MAD_SOCKET should win, got %q", got)
	}
}

func TestLoadSessionAndToken(t *testing.T) {
	cases := []struct {
		name              string
		session, token    string
		wantSess, wantTok string
	}{
		{"plain", "s-123", "tok-abc", "s-123", "tok-abc"},
		{"trims whitespace", "  s-123  ", "\ttok\n", "s-123", "tok"},
		{"empty stays empty", "   ", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("MAD_RUNTIME_DIR", t.TempDir())
			t.Setenv("MAD_SESSION", tc.session)
			t.Setenv("MAD_SESSION_TOKEN", tc.token)
			cfg := Load()
			if cfg.Session != tc.wantSess {
				t.Errorf("Session: want %q, got %q", tc.wantSess, cfg.Session)
			}
			if cfg.Token != tc.wantTok {
				t.Errorf("Token: want %q, got %q", tc.wantTok, cfg.Token)
			}
		})
	}
}

func TestLoadDurations(t *testing.T) {
	cases := []struct {
		name           string
		ttl, rpc       string
		wantTTL, wantR time.Duration
	}{
		{"defaults when unset", "", "", defaultLeaseTTL, defaultRPCTimeout},
		{"valid positive", "30000", "500", 30 * time.Second, 500 * time.Millisecond},
		{"zero falls back", "0", "0", defaultLeaseTTL, defaultRPCTimeout},
		{"negative falls back", "-5", "-1", defaultLeaseTTL, defaultRPCTimeout},
		{"unparseable falls back", "abc", "1.5", defaultLeaseTTL, defaultRPCTimeout},
		{"whitespace trimmed", "  90000 ", " 250 ", 90 * time.Second, 250 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("MAD_RUNTIME_DIR", t.TempDir())
			t.Setenv("MAD_LEASE_TTL_MS", tc.ttl)
			t.Setenv("MAD_RPC_TIMEOUT_MS", tc.rpc)
			cfg := Load()
			if cfg.LeaseTTL != tc.wantTTL {
				t.Errorf("LeaseTTL for %q: want %s, got %s", tc.ttl, tc.wantTTL, cfg.LeaseTTL)
			}
			if cfg.RPCTimeout != tc.wantR {
				t.Errorf("RPCTimeout for %q: want %s, got %s", tc.rpc, tc.wantR, cfg.RPCTimeout)
			}
		})
	}
}

func TestErrorIsTransport(t *testing.T) {
	transport := &Error{Code: 0, Message: "dial: connection refused"}
	if !transport.IsTransport() {
		t.Error("Code 0 should be transport")
	}
	if !IsTransport(transport) {
		t.Error("IsTransport(Code 0) should be true")
	}

	verdict := &Error{Code: protocol.CodeAuthz, Message: "authz"}
	if verdict.IsTransport() {
		t.Error("CodeAuthz should NOT be transport")
	}
	if IsTransport(verdict) {
		t.Error("IsTransport(CodeAuthz) should be false")
	}

	// A non-*Error / nil error is never a transport error.
	if IsTransport(errors.New("plain")) {
		t.Error("plain error should not be transport")
	}
	if IsTransport(nil) {
		t.Error("nil should not be transport")
	}

	// errors.As must see through a wrap.
	wrapped := fmt.Errorf("context: %w", transport)
	if !IsTransport(wrapped) {
		t.Error("wrapped transport *Error should be transport")
	}
}

// TestClassifyErr verifies the structured code is recovered from rpcclient's
// flattened "(code N)" suffix, and that a suffix-less error is transport.
func TestClassifyErr(t *testing.T) {
	// Shape exactly as rpcclient formats a protocol error.
	protoErr := fmt.Errorf("rpc lease.acquire: %s (code %d)", "denied", protocol.CodeConflict)
	got := classifyErr(protoErr)
	var ce *Error
	if !errors.As(got, &ce) {
		t.Fatalf("want *Error, got %T", got)
	}
	if ce.Code != protocol.CodeConflict {
		t.Errorf("code: want %d, got %d", protocol.CodeConflict, ce.Code)
	}
	if ce.IsTransport() {
		t.Error("a coded error must not be transport")
	}

	// Transport error from rpcclient (dial/decode) has NO "(code N)" suffix.
	transErr := fmt.Errorf("rpcclient: dial /x.sock: connection refused")
	tg := classifyErr(transErr)
	if !IsTransport(tg) {
		t.Errorf("suffix-less error should be transport, got %v", tg)
	}

	if classifyErr(nil) != nil {
		t.Error("classifyErr(nil) should be nil")
	}
}
