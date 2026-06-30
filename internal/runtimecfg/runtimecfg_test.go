package runtimecfg

import (
	"os"
	"path/filepath"
	"testing"
)

// clearEnv wipes every input env var so each case starts from a known floor.
// t.Setenv("", ...) is not valid, so we explicitly clear (not Setenv "") the
// ones a case wants absent; t.Setenv restores all of them after the test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MAD_SOCKET", "MAD_RUNTIME_DIR", "MAD_HOME"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// NOTE: no t.Parallel anywhere — every case mutates process env via t.Setenv.

func TestSocketPathPrecedence(t *testing.T) {
	t.Run("flag beats everything", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MAD_SOCKET", "/env/socket.sock")
		t.Setenv("MAD_RUNTIME_DIR", "/rd")
		t.Setenv("MAD_HOME", "/home")
		if got := SocketPath("/flag/explicit.sock"); got != "/flag/explicit.sock" {
			t.Fatalf("flag should win, got %q", got)
		}
	})

	t.Run("MAD_SOCKET beats runtime dir", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MAD_SOCKET", "/env/socket.sock")
		t.Setenv("MAD_RUNTIME_DIR", "/rd")
		t.Setenv("MAD_HOME", "/home")
		if got := SocketPath(""); got != "/env/socket.sock" {
			t.Fatalf("MAD_SOCKET should win, got %q", got)
		}
	})

	t.Run("RUNTIME_DIR beats HOME", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MAD_RUNTIME_DIR", "/rd")
		t.Setenv("MAD_HOME", "/home")
		want := filepath.Join("/rd", "daemon.sock")
		if got := SocketPath(""); got != want {
			t.Fatalf("RUNTIME_DIR should win, want %q got %q", want, got)
		}
	})

	t.Run("HOME beats default", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MAD_HOME", "/home")
		want := filepath.Join("/home", "daemon.sock")
		if got := SocketPath(""); got != want {
			t.Fatalf("HOME should win, want %q got %q", want, got)
		}
	})

	t.Run("default falls back to ~/.mad-substrate", func(t *testing.T) {
		clearEnv(t)
		// Use a scratch HOME via UserHomeDir's backing var so we don't MkdirAll
		// into the real home. On unix UserHomeDir reads $HOME.
		scratch := t.TempDir()
		t.Setenv("HOME", scratch)
		want := filepath.Join(scratch, ".mad-substrate", "daemon.sock")
		if got := SocketPath(""); got != want {
			t.Fatalf("default want %q got %q", want, got)
		}
		// The default branch MkdirAlls the runtime dir.
		if fi, err := os.Stat(filepath.Join(scratch, ".mad-substrate")); err != nil || !fi.IsDir() {
			t.Fatalf("default branch must ensure runtime dir exists: %v", err)
		}
	})
}

func TestEmptyAndWhitespaceEnvIgnored(t *testing.T) {
	clearEnv(t)
	scratch := t.TempDir()
	t.Setenv("HOME", scratch)
	// Whitespace-only values must be treated as unset.
	t.Setenv("MAD_SOCKET", "   ")
	t.Setenv("MAD_RUNTIME_DIR", "\t")
	t.Setenv("MAD_HOME", " ")
	want := filepath.Join(scratch, ".mad-substrate", "daemon.sock")
	if got := SocketPath(""); got != want {
		t.Fatalf("whitespace env should be ignored, want %q got %q", want, got)
	}
	// A whitespace-only flag override is also ignored.
	if got := SocketPath("   "); got != want {
		t.Fatalf("whitespace flag should be ignored, want %q got %q", want, got)
	}
	// Resolved socket should be trimmed when a real env value has padding.
	t.Setenv("MAD_SOCKET", "  /padded.sock  ")
	if got := SocketPath(""); got != "/padded.sock" {
		t.Fatalf("env value should be trimmed, got %q", got)
	}
}

func TestSocketSource(t *testing.T) {
	cases := []struct {
		name       string
		flag       string
		setEnv     map[string]string
		wantPath   string
		wantSource string
	}{
		{"flag", "/f.sock", nil, "/f.sock", SourceFlag},
		{"env socket", "", map[string]string{"MAD_SOCKET": "/e.sock"}, "/e.sock", SourceEnvSocket},
		{"runtime dir", "", map[string]string{"MAD_RUNTIME_DIR": "/rd"}, filepath.Join("/rd", "daemon.sock"), SourceRuntimeDir},
		{"home", "", map[string]string{"MAD_HOME": "/h"}, filepath.Join("/h", "daemon.sock"), SourceHome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.setEnv {
				t.Setenv(k, v)
			}
			path, source := SocketSource(tc.flag)
			if path != tc.wantPath || source != tc.wantSource {
				t.Fatalf("SocketSource(%q) = (%q,%q), want (%q,%q)", tc.flag, path, source, tc.wantPath, tc.wantSource)
			}
		})
	}

	t.Run("default source", func(t *testing.T) {
		clearEnv(t)
		scratch := t.TempDir()
		t.Setenv("HOME", scratch)
		path, source := SocketSource("")
		if source != SourceDefault {
			t.Fatalf("want default source, got %q", source)
		}
		if path != filepath.Join(scratch, ".mad-substrate", "daemon.sock") {
			t.Fatalf("unexpected default path %q", path)
		}
	})
}

// SocketSource must be side-effect-free (no MkdirAll) even on the default branch.
func TestSocketSourceNoSideEffect(t *testing.T) {
	clearEnv(t)
	scratch := t.TempDir()
	t.Setenv("HOME", scratch)
	_, _ = SocketSource("")
	if _, err := os.Stat(filepath.Join(scratch, ".mad-substrate")); !os.IsNotExist(err) {
		t.Fatalf("SocketSource must not create the runtime dir, stat err=%v", err)
	}
}

func TestRuntimeDirSource(t *testing.T) {
	clearEnv(t)
	t.Setenv("MAD_RUNTIME_DIR", "/rd")
	if dir, src := RuntimeDirSource(); dir != "/rd" || src != SourceRuntimeDir {
		t.Fatalf("got (%q,%q)", dir, src)
	}
	clearEnv(t)
	t.Setenv("MAD_HOME", "/h")
	if dir, src := RuntimeDirSource(); dir != "/h" || src != SourceHome {
		t.Fatalf("got (%q,%q)", dir, src)
	}
}

func TestRuntimeDirEnsuresDir(t *testing.T) {
	clearEnv(t)
	rd := filepath.Join(t.TempDir(), "nested", "rt")
	t.Setenv("MAD_RUNTIME_DIR", rd)
	if got := RuntimeDir(); got != rd {
		t.Fatalf("RuntimeDir want %q got %q", rd, got)
	}
	if fi, err := os.Stat(rd); err != nil || !fi.IsDir() {
		t.Fatalf("RuntimeDir must MkdirAll the dir: %v", err)
	}
}

func TestDivergence(t *testing.T) {
	t.Run("both set and differ", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MAD_RUNTIME_DIR", "/rd")
		t.Setenv("MAD_HOME", "/home")
		rd, hm, div := Divergence()
		if rd != "/rd" || hm != "/home" || !div {
			t.Fatalf("expected divergence, got (%q,%q,%v)", rd, hm, div)
		}
	})
	t.Run("both set and equal — no divergence", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MAD_RUNTIME_DIR", "/same")
		t.Setenv("MAD_HOME", "/same")
		if _, _, div := Divergence(); div {
			t.Fatal("equal values must not diverge")
		}
	})
	t.Run("only one set — no divergence", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MAD_RUNTIME_DIR", "/rd")
		if _, _, div := Divergence(); div {
			t.Fatal("single var must not diverge")
		}
	})
	t.Run("whitespace counts as unset", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MAD_RUNTIME_DIR", "/rd")
		t.Setenv("MAD_HOME", "   ")
		if _, _, div := Divergence(); div {
			t.Fatal("whitespace HOME must not diverge")
		}
	})
}
