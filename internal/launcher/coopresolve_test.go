package launcher

import (
	"os"
	"testing"
)

// TestResolveRelayHostPath_EnvWinsOverEmbedded pins precedence #1: when BOTH the
// MAD_CONTAINER_RELAY override AND an embedded payload are available, the
// override WINS and the resolver does NOT own a temp file (nil cleanup).
//
// NON-VACUOUS CONTROL: an embedded payload IS injected here, so a regression that
// preferred the embedded relay would flip this red (path would be a temp file, not
// the override path, and cleanup would be non-nil).
func TestResolveRelayHostPath_EnvWinsOverEmbedded(t *testing.T) {
	prev := relayBytesFn
	relayBytesFn = func(string) ([]byte, bool) { return []byte("EMBEDDED-PAYLOAD"), true }
	t.Cleanup(func() { relayBytesFn = prev })

	t.Setenv(coopRelayEnv, "/host/relay/override")
	path, cleanup, err := resolveRelayHostPath("arm64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
		t.Fatal("env override must NOT own a temp file (cleanup must be nil)")
	}
	if path != "/host/relay/override" {
		t.Fatalf("env override must win; got %q", path)
	}
}

// TestResolveRelayHostPath_EmbeddedWhenNoEnv pins precedence #2: with NO override,
// the embedded payload is materialized to an executable temp host file whose bytes
// match, and the returned cleanup removes it.
func TestResolveRelayHostPath_EmbeddedWhenNoEnv(t *testing.T) {
	prev := relayBytesFn
	want := []byte("EMBEDDED-RELAY-BYTES")
	relayBytesFn = func(arch string) ([]byte, bool) {
		if arch != "arm64" {
			return nil, false
		}
		return want, true
	}
	t.Cleanup(func() { relayBytesFn = prev })

	t.Setenv(coopRelayEnv, "") // treated as unset (TrimSpace == "")
	path, cleanup, err := resolveRelayHostPath("arm64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if path == "" || cleanup == nil {
		t.Fatalf("embedded payload must resolve to an owned temp path; got (%q, cleanup=%v)", path, cleanup != nil)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read staged relay: %v", rerr)
	}
	if string(got) != string(want) {
		t.Fatalf("staged relay bytes = %q; want %q", got, want)
	}
	fi, serr := os.Stat(path)
	if serr != nil {
		t.Fatalf("stat staged relay: %v", serr)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("staged relay must be executable, mode=%v", fi.Mode())
	}
	// NON-VACUOUS: prove cleanup actually removes the temp file.
	cleanup()
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatalf("cleanup must remove the temp relay; stat err=%v", e)
	}
}

// TestResolveRelayHostPath_NoneAvailable pins the fail-soft default: no override and
// no embedded payload (the untagged stub) resolves to ("", nil, nil) — the caller's
// "run confined without the plane" path. This is the control proving the embedded
// branch above is reached ONLY because a payload was injected.
func TestResolveRelayHostPath_NoneAvailable(t *testing.T) {
	prev := relayBytesFn
	relayBytesFn = func(string) ([]byte, bool) { return nil, false }
	t.Cleanup(func() { relayBytesFn = prev })

	t.Setenv(coopRelayEnv, "")
	path, cleanup, err := resolveRelayHostPath("arm64")
	if err != nil || path != "" || cleanup != nil {
		t.Fatalf("none-available must be (\"\", nil, nil); got (%q, cleanup=%v, %v)", path, cleanup != nil, err)
	}
}
