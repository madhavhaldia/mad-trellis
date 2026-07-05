//go:build packaging

// Package packaging holds the HAND-AUTHORED Stage-D packaging guards for project
// 10b (distribution-packaging). The slow, host-specific guards here carry
// `//go:build packaging` so the normal `go test ./...` sweep skips them; they run
// only under `go test -tags packaging ./internal/packaging/` (the Makefile
// linkage/smoke targets do exactly this). They shell out to the Go toolchain and
// otool/ldd/readelf and assert OS-level facts no unit test can. The PURE linkage
// predicates they feed live in the untagged linkage.go, unit-tested by the
// untagged predicates_test.go on every host.
//
// linkage_test.go is the cgo carve-out guard. mad-trellis depends on the PURE-Go
// modernc.org/sqlite (a Go MODULE dependency) precisely so the shipped binary is
// cgo-free — it links ONLY system libraries, never a bundled libsqlite3 or any
// homebrew/third-party C library. This file proves that twice over:
//
//   - TestLinkageCgoFree builds the real release binary CGO_ENABLED=0 and asserts
//     its dynamic linkage is exclusively system libs AND that modernc.org/sqlite
//     is nonetheless an embedded module dependency (the carve-out point: a SQLite
//     in the binary that contributes ZERO C linkage).
//   - TestLinkageControlDetectsCgo is the C24 NON-VACUOUS control: it builds a
//     deliberately cgo-using program and asserts the SAME authoritative signal
//     flips to not-cgo-free — so a green TestLinkageCgoFree genuinely means
//     something.
//
// CROSS-GOOS: the AUTHORITATIVE verdict is binaryCgoEnabled (the binary's embedded
// `build CGO_ENABLED=` setting), which is read from `go version -m` and is therefore
// OS-INDEPENDENT — it runs and is asserted on every GOOS. The linkage scan is the
// complementary, per-OS defense-in-depth check: on darwin it shells out to
// `otool -L`, on linux to `ldd`/`readelf -d`. linkageScan dispatches on
// runtime.GOOS; if the OS-specific tool is absent it SKIPS that sub-check with a
// reason (never a silent pass), while the authoritative CGO_ENABLED check still runs.
package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory until it finds go.mod, so
// the build commands below can run with cmd.Dir set to the module root (matching
// internal/buildinfo/drift_test.go's helper of the same name).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod walking up from %s", wd)
		}
		dir = parent
	}
}

// linkageScan runs the OS-specific dynamic-linkage check on bin and reports any
// offending (non-system / third-party / bundled-C) libraries. It dispatches on
// runtime.GOOS:
//
//   - darwin: `otool -L` parsed by isCgoFreeLinkage (allow /usr/lib + /System/Library;
//     reject /opt/homebrew, /usr/local, @rpath, @loader_path, any libsqlite3/libsql).
//   - linux:  `ldd` (or `readelf -d` DT_NEEDED) parsed by isCgoFreeLinkageLinux
//     (allow the linux system set: linux-vdso, ld-linux, libc/libpthread/libdl/libm/
//     librt; reject any third-party/bundled C such as libsqlite3.so).
//
// Returns:
//
//	offending   — the non-system libraries found (empty when ok).
//	tool        — the OS tool actually used (for diagnostics), or "" when skipped.
//	ok          — true iff the scan ran AND found no offending library.
//	skipReason  — non-empty iff the scan could not run (OS tool absent, or GOOS not
//	              covered); the caller SKIPS the sub-check loudly. ok is false in this
//	              case, so a skip is never mistaken for a clean pass.
//
// This is complementary defense-in-depth only: the AUTHORITATIVE cgo verdict is
// binaryCgoEnabled, which is OS-independent and always asserted. The pure parsers
// (isCgoFreeLinkage / isCgoFreeLinkageLinux) live in linkage.go.
func linkageScan(bin string) (offending []string, tool string, ok bool, skipReason string) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("otool", "-L", bin).CombinedOutput()
		if err != nil {
			return nil, "otool", false, "otool -L unavailable (skipping the darwin dylib scan; the authoritative CGO_ENABLED check still ran): " + err.Error()
		}
		if strings.TrimSpace(string(out)) == "" {
			return nil, "otool", false, "otool -L produced no output (cannot verify darwin linkage)"
		}
		ok, offending = isCgoFreeLinkage(string(out))
		return offending, "otool", ok, ""
	case "linux":
		// Prefer ldd; fall back to readelf -d (DT_NEEDED). A cgo-free static-ish Go
		// binary makes ldd say "not a dynamic executable" (ok with no offending),
		// or list only the linux system set.
		if _, err := exec.LookPath("ldd"); err == nil {
			out, err := exec.Command("ldd", bin).CombinedOutput()
			// ldd exits non-zero with "not a dynamic executable" for a fully static
			// binary; that is a CLEAN result, so parse the output rather than treat
			// the non-zero exit as a tool failure.
			ok, offending = isCgoFreeLinkageLinux(string(out), false)
			_ = err
			return offending, "ldd", ok, ""
		}
		if _, err := exec.LookPath("readelf"); err == nil {
			out, err := exec.Command("readelf", "-d", bin).CombinedOutput()
			if err != nil {
				return nil, "readelf", false, "readelf -d failed (cannot verify linux linkage): " + err.Error()
			}
			ok, offending = isCgoFreeLinkageLinux(string(out), true)
			return offending, "readelf", ok, ""
		}
		return nil, "", false, "neither ldd nor readelf available (skipping the linux ELF scan; the authoritative CGO_ENABLED check still ran)"
	default:
		return nil, "", false, "no linkage scan implemented for GOOS=" + runtime.GOOS + " (skipping the OS scan; the authoritative CGO_ENABLED check still ran)"
	}
}

// binaryCgoEnabled returns the AUTHORITATIVE cgo setting embedded in a built
// binary: the value of the `build CGO_ENABLED=...` line in `go version -m`. This
// is the ground truth for "was this binary built cgo-free" — independent of any
// platform-specific linkage heuristic. Returns "" if the setting is absent.
func binaryCgoEnabled(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command("go", "version", "-m", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m %s failed: %v\n%s", bin, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, tok := range strings.Fields(line) {
			if strings.HasPrefix(tok, "CGO_ENABLED=") {
				return strings.TrimPrefix(tok, "CGO_ENABLED=")
			}
		}
	}
	return ""
}

// TestLinkageCgoFree builds the real release binary CGO_ENABLED=0 and asserts (1)
// its embedded build info reports CGO_ENABLED=0 — the AUTHORITATIVE, OS-INDEPENDENT
// cgo verdict; (2) the per-OS linkage scan finds no third-party/bundled C library
// (darwin otool / linux ldd|readelf, via linkageScan — SKIPPED loudly if the OS
// tool is absent, never a silent pass); (3) no sqlite/libsql library is linked; and
// (4) modernc.org/sqlite is still a module dependency — the cgo carve-out made
// concrete (a SQLite that contributes ZERO C linkage).
func TestLinkageCgoFree(t *testing.T) {
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "mad-trellis")

	// Build cgo-free + -trimpath (reproducible) straight from ./cmd/mad-trellis.
	build := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/mad-trellis")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 build of ./cmd/mad-trellis failed: %v\n%s", err, out)
	}

	// (1) AUTHORITATIVE (OS-INDEPENDENT): the binary's embedded build setting must
	// say cgo-free. This is the load-bearing verdict on every GOOS.
	if cgo := binaryCgoEnabled(t, bin); cgo != "0" {
		t.Errorf("binary build info reports CGO_ENABLED=%q, want \"0\" (binary is not cgo-free)", cgo)
	}

	// (2)+(3) Per-OS defense in depth: run the GOOS-aware linkage scan. On darwin
	// every linked dylib must be a system lib (frameworks CoreFoundation/Security
	// ARE allowed — pure-Go crypto/x509|tls link them without cgo); on linux a
	// static cgo-free Go binary reports "not a dynamic executable" or only the
	// system set. If the OS tool is unavailable we SKIP this sub-check loudly — the
	// authoritative CGO_ENABLED check above already ran.
	offending, tool, ok, skipReason := linkageScan(bin)
	if skipReason != "" {
		t.Logf("linkage scan skipped (OS=%s): %s", runtime.GOOS, skipReason)
	} else {
		if !ok {
			t.Errorf("binary links non-system dynamic libraries (third-party/bundled C) per %s on %s: %v", tool, runtime.GOOS, offending)
		}
		// (3) No referenced library may name sqlite/libsql (case-insensitive): a cgo
		// SQLite would link a libsqlite3/libsql dylib/so, which is exactly what the
		// pure-Go driver avoids.
		for _, lib := range offending {
			low := strings.ToLower(lib)
			if strings.Contains(low, "sqlite") || strings.Contains(low, "libsql") {
				t.Errorf("%s names a sqlite/libsql library (cgo SQLite leaked into the linkage): %q", tool, lib)
			}
		}
	}

	// The carve-out point: modernc.org/sqlite IS a Go module dependency, yet (per
	// the linkage assertions above) contributes ZERO C linkage — pure-Go SQLite.
	// `go version -m` is OS-independent, so this runs on every GOOS.
	mods, err := exec.Command("go", "version", "-m", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m %s failed: %v\n%s", bin, err, mods)
	}
	if !strings.Contains(string(mods), "modernc.org/sqlite") {
		t.Errorf("modernc.org/sqlite not found in `go version -m` of the binary — the cgo carve-out assumes the pure-Go sqlite is the SQLite dependency:\n%s", mods)
	}
}

// TestLinkageControlDetectsCgo is the C24 non-vacuous control. It builds a tiny,
// genuinely cgo-using program with CGO_ENABLED=1 and asserts the SAME authoritative
// signal (binaryCgoEnabled) that TestLinkageCgoFree relies on FLIPS to "1" (not
// cgo-free). This proves the signal actually distinguishes a cgo binary from a
// cgo-free one, so a green TestLinkageCgoFree is meaningful rather than vacuously
// passing. The flip is OS-INDEPENDENT (it reads the binary's embedded build info),
// so the control is load-bearing on every GOOS. The per-OS linkageScan is recorded
// as secondary, informational evidence only.
//
// If the cgo build cannot run (e.g. no C compiler / SDK in the environment) the
// test SKIPS with a clear reason — never a silent false pass.
func TestLinkageControlDetectsCgo(t *testing.T) {
	dir := t.TempDir()

	// A minimal module whose main genuinely uses cgo (it #includes a C header and
	// calls into C). Building this CGO_ENABLED=1 forces the linker to pull in a C
	// runtime dylib, which the predicate must flag — OR the build fails for lack
	// of a toolchain, in which case we skip.
	goMod := "module cgoctl\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	mainGo := `package main

/*
#include <stdlib.h>
*/
import "C"

func main() {
	// A real cgo use so the build genuinely links the C runtime: allocate and
	// free one byte through libc. The C symbols guarantee a cgo (non-pure-Go)
	// binary that the linkage predicate must flag as not-cgo-free.
	p := C.malloc(C.size_t(1))
	C.free(p)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	bin := filepath.Join(dir, "cgoctl")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		// No C compiler / SDK -> we cannot exercise the control. Skip LOUDLY with
		// the build output so this is never mistaken for a pass.
		t.Skipf("cgo build unavailable in this environment (cannot run the non-vacuous control); skipping rather than silently passing: %v\n%s", err, out)
	}

	// AUTHORITATIVE non-vacuity check: a CGO_ENABLED=1 build must report
	// CGO_ENABLED=1 in its embedded build info — so the SAME signal
	// TestLinkageCgoFree relies on (binaryCgoEnabled == "0") correctly FLIPS to
	// "not cgo-free" here. This is robust regardless of which dylibs the cgo binary
	// happens to link (a libc-only cgo binary may link only /usr/lib + an allowed
	// framework, so the linkage scan alone would not flip — the build setting is
	// the load-bearing signal).
	if cgo := binaryCgoEnabled(t, bin); cgo != "1" {
		t.Fatalf("CONTROL FAILED: a CGO_ENABLED=1 build reported CGO_ENABLED=%q; the authoritative cgo signal does not flip, so TestLinkageCgoFree's cgo check would be VACUOUS.", cgo)
	}
	t.Logf("control OK: cgo binary reports CGO_ENABLED=1 (the authoritative cgo signal flips)")

	// Secondary evidence (per-OS, informational only): if the cgo binary links a
	// non-system (third-party/bundled C) library, the GOOS-aware linkage scan flags
	// it too. The authoritative signal above is the load-bearing flip; a libc-only
	// cgo binary may link only allowed system libs, so this scan alone need not flip.
	if offending, tool, ok, skipReason := linkageScan(bin); skipReason != "" {
		t.Logf("control note: linkage scan skipped (%s): %s", runtime.GOOS, skipReason)
	} else if !ok {
		t.Logf("control note: %s linkage scan also flagged non-system libs: %v", tool, offending)
	} else {
		t.Logf("control note: %s linkage scan found only system libs (the cgo binary links libc-only here; the authoritative flip is the load-bearing check)", tool)
	}
}

// TestCgoFreeBuildSucceeds asserts the CGO_ENABLED=0 build of ./cmd/mad-trellis
// exits 0. A cgo import sneaking into the dependency graph (e.g. swapping the
// pure-Go sqlite for a cgo driver) would make this build fail, catching the
// regression before it ever reaches the linkage assertions above.
func TestCgoFreeBuildSucceeds(t *testing.T) {
	root := repoRoot(t)
	build := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "mad-trellis"), "./cmd/mad-trellis")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 go build ./cmd/mad-trellis must succeed (a cgo import would break it): %v\n%s", err, out)
	}
}
