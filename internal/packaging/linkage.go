// Pure linkage predicates for the cgo carve-out guard (project 10b). These live
// in an UNTAGGED file (no //go:build packaging) so they are part of the normal
// package build and can be exercised by untagged table-driven tests
// (predicates_test.go) on EVERY host — including the linux branch on a darwin dev
// box, closing the gap that the linux scan was previously only validated by a
// throwaway test. The slow, host-specific drivers that FEED them real
// otool/ldd/readelf output (linkageScan, binaryCgoEnabled, and the binary-building
// Test* funcs) stay in the //go:build packaging file linkage_test.go.

package packaging

import "strings"

// isCgoFreeLinkage scans `otool -L` output (DARWIN) and reports whether EVERY
// linked dynamic library is one a darwin Go binary may legitimately link: a plain
// dylib under /usr/lib/, or a /System/Library/ entry (the macOS frameworks).
// Anything else — a homebrew/third-party C lib (/opt/homebrew/*, /usr/local/*), an
// @rpath/@loader_path bundled lib, or a bundled libsqlite3/libsql — is reported
// in offending (making ok=false). This is the darwin THIRD-PARTY/bundled-C guard.
//
// IMPORTANT (this corrects an earlier framework-as-cgo heuristic that produced a
// FALSE RED): a pure-Go (CGO_ENABLED=0) darwin binary that imports crypto/x509,
// crypto/tls, or net/http DOES link the CoreFoundation + Security frameworks —
// via the stdlib's //go:cgo_import_dynamic lazy-linker directives, NOT via cgo.
// So "links a framework" is NOT equivalent to "is a cgo binary" on darwin, and
// treating a framework as a red flag would false-RED a genuinely cgo-free binary
// the moment it gained a TLS/HTTPS/cert dependency. The AUTHORITATIVE cgo signal
// is the binary's embedded `build CGO_ENABLED` setting (see binaryCgoEnabled);
// this dylib scan is the complementary guard that no third-party/bundled C
// library (what a cgo SQLite would drag in) is linked. TestLinkageCgoFree asserts
// BOTH; the C24 control flips on the authoritative signal.
//
// Non-vacuity: if otool produced no parseable dylib line at all (empty/garbage),
// ok is false — an empty scan must never read as "clean".
//
// otool -L output shape (first line is the binary path itself, then one indented
// line per linked dylib):
//
//	/path/to/bin:
//		/usr/lib/libSystem.B.dylib (compatibility version ...)
//		/System/Library/Frameworks/CoreFoundation.framework/... (compatibility ...)
//		/opt/homebrew/opt/sqlite/lib/libsqlite3.0.dylib (compatibility ...)
func isCgoFreeLinkage(otoolOutput string) (ok bool, offending []string) {
	lines := strings.Split(otoolOutput, "\n")
	sawDylib := false
	for i, raw := range lines {
		// The very first non-empty line is "<binary path>:" — the subject, not a
		// linked lib. Skip it (and any blank lines).
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if i == 0 && strings.HasSuffix(trimmed, ":") {
			continue
		}
		// A linked-lib line names a path followed by " (compatibility version ...)".
		// Take the first field. otool emits only the subject + indented entries, so
		// every remaining non-empty line is a dylib entry (we do NOT skip a
		// non-indented line — garbage must not be silently dropped).
		lib := trimmed
		if idx := strings.Index(lib, " "); idx >= 0 {
			lib = lib[:idx]
		}
		if lib == "" {
			continue
		}
		sawDylib = true
		if isSystemDylib(lib) {
			continue
		}
		offending = append(offending, lib)
	}
	if !sawDylib {
		// No linked library parsed at all — otool output was empty/garbage. A
		// vacuous "clean" here would defeat the guard; report it as not-ok.
		return false, []string{"(no linked libraries found in otool output)"}
	}
	return len(offending) == 0, offending
}

// isSystemDylib reports whether a linked library is one a DARWIN Go binary may
// link WITHOUT dragging in a third-party/bundled C dependency: a plain dylib
// under /usr/lib/, or a /System/Library/ entry (the macOS frameworks, which
// pure-Go crypto/x509|tls and net/http link via the stdlib's
// //go:cgo_import_dynamic directives). A homebrew/@rpath/@loader_path//usr/local
// path — or a bundled libsqlite3 — is NOT a system dylib and is flagged.
func isSystemDylib(path string) bool {
	return strings.HasPrefix(path, "/usr/lib/") || strings.HasPrefix(path, "/System/Library/")
}

// isSystemSharedObjectLinux reports whether a DT_NEEDED / ldd library name (the
// soname or path) is part of the linux system set a cgo-FREE Go binary may
// legitimately reference: the dynamic loader and the glibc/musl core. Anything
// else — most importantly a bundled/third-party C library such as libsqlite3.so —
// is NOT a system object and is flagged.
//
// We match on the BASENAME's soname prefix so this is robust to either an absolute
// path (`/lib/x86_64-linux-gnu/libc.so.6`) or a bare soname (`libc.so.6`), and to
// the trailing version suffix (`.so.6`, `.so.1`). The allowlist is deliberately
// tight: only the loader, libc/musl, and the historically-separate glibc shards a
// pure-Go binary's tiny dynamic surface can pull in (pthread/dl/m/rt/util — folded
// into libc on modern glibc but still possible as sonames on older systems). A
// pure-Go (CGO_ENABLED=0) net/os.user binary on glibc may reference libc via the
// NSS/getaddrinfo dynamic-link path; that is NOT cgo and is allowed here.
func isSystemSharedObjectLinux(lib string) bool {
	base := lib
	if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
		base = base[idx+1:]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return false
	}
	// linux-vdso / linux-gate is a kernel-provided virtual DSO, never a file.
	if strings.HasPrefix(base, "linux-vdso") || strings.HasPrefix(base, "linux-gate") {
		return true
	}
	// The dynamic loader itself: ld-linux*.so.*, ld-musl-*.so.*, ld.so.*.
	if strings.HasPrefix(base, "ld-linux") || strings.HasPrefix(base, "ld-musl") || strings.HasPrefix(base, "ld.so") {
		return true
	}
	// The glibc/musl core sonames a cgo-free Go binary may legitimately pull in.
	allowedSonames := []string{
		"libc.so", "libc.musl",
		"libpthread.so", "libdl.so", "libm.so", "librt.so", "libutil.so",
		"libresolv.so", "libnss_",
	}
	for _, s := range allowedSonames {
		if strings.HasPrefix(base, s) {
			return true
		}
	}
	return false
}

// isCgoFreeLinkageLinux scans linux dynamic-dependency output and reports whether
// EVERY referenced shared object is part of the linux system set
// (isSystemSharedObjectLinux). It accepts BOTH input shapes:
//
//   - ldd (fromReadelf=false): one line per dependency, e.g.
//     linux-vdso.so.1 (0x00007fff...)
//     libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x00007f...)
//     /lib64/ld-linux-x86-64.so.2 (0x00007f...)
//     A fully static cgo-free Go binary instead prints exactly:
//     not a dynamic executable
//     which is a CLEAN result (no dependencies → no offending libs).
//   - readelf -d (fromReadelf=true): the .dynamic section, where each NEEDED entry
//     reads like:
//     0x0000000000000001 (NEEDED)  Shared library: [libc.so.6]
//     A fully static binary has no NEEDED entries (also clean).
//
// A bundled/third-party C library (esp. libsqlite3.so — what a cgo SQLite would
// drag in) is reported in offending, making ok=false. Unlike the darwin scan there
// is no "saw at least one lib" non-vacuity requirement: a legitimately STATIC
// cgo-free linux Go binary has ZERO dynamic dependencies, so "no libs" is the
// expected clean state, not a vacuous pass — the authoritative non-vacuity guard
// remains binaryCgoEnabled / the C24 control.
func isCgoFreeLinkageLinux(out string, fromReadelf bool) (ok bool, offending []string) {
	low := strings.ToLower(out)
	if !fromReadelf && strings.Contains(low, "not a dynamic executable") {
		// Fully static binary: no dynamic dependencies at all. Clean.
		return true, nil
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var lib string
		if fromReadelf {
			// Only NEEDED entries name a dependency: "... (NEEDED) Shared library: [name]".
			if !strings.Contains(line, "(NEEDED)") {
				continue
			}
			lo := strings.IndexByte(line, '[')
			hi := strings.IndexByte(line, ']')
			if lo < 0 || hi <= lo {
				continue
			}
			lib = strings.TrimSpace(line[lo+1 : hi])
		} else {
			// ldd line: "<soname> => <path> (0x...)" or "<path-or-vdso> (0x...)".
			// Take the resolved path after "=>" when present (more specific), else
			// the leading token.
			if idx := strings.Index(line, "=>"); idx >= 0 {
				rhs := strings.TrimSpace(line[idx+2:])
				// Strip the trailing "(0x...)" address annotation.
				if p := strings.Index(rhs, " ("); p >= 0 {
					rhs = strings.TrimSpace(rhs[:p])
				}
				lib = rhs
				if lib == "" || lib == "not found" {
					// Unresolved: fall back to the requested soname (lhs).
					lib = strings.TrimSpace(line[:idx])
				}
			} else {
				lib = line
				if p := strings.Index(lib, " ("); p >= 0 {
					lib = strings.TrimSpace(lib[:p])
				}
			}
		}
		if lib == "" {
			continue
		}
		if isSystemSharedObjectLinux(lib) {
			continue
		}
		offending = append(offending, lib)
	}
	return len(offending) == 0, offending
}
