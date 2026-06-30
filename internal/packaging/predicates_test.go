// Untagged unit tests for the pure linkage predicates in linkage.go. NO
// //go:build packaging here, so these run in the normal `go test ./...` sweep on
// EVERY host — they feed the parsers synthetic otool/ldd/readelf strings, so the
// linux branch is exercised on a darwin dev box (and vice versa) without a real
// linux host. They cover BOTH the accept (clean) and reject (flagged) paths so the
// release guard's flag-path is self-validating rather than verified only by
// inspection. The host-specific drivers (linkageScan, the binary-building tests)
// remain in the //go:build packaging linkage_test.go.
package packaging

import "testing"

func TestIsSystemDylib_Darwin(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/usr/lib/libSystem.B.dylib", true},
		{"/usr/lib/libresolv.9.dylib", true},
		{"/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation", true},
		{"/System/Library/Frameworks/Security.framework/Versions/A/Security", true},
		// Third-party / bundled C — must NOT be treated as system.
		{"/opt/homebrew/opt/sqlite/lib/libsqlite3.0.dylib", false},
		{"/usr/local/lib/libfoo.dylib", false},
		{"@rpath/libbar.dylib", false},
		{"@loader_path/libbaz.dylib", false},
		{"/Users/dev/vendored/libsql.dylib", false},
	}
	for _, c := range cases {
		if got := isSystemDylib(c.path); got != c.want {
			t.Errorf("isSystemDylib(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIsCgoFreeLinkage_Darwin(t *testing.T) {
	// A clean pure-Go darwin binary: subject line + only system libs/frameworks.
	clean := "/tmp/mad-substrate:\n" +
		"\t/usr/lib/libSystem.B.dylib (compatibility version 1.0.0, current version 1.0.0)\n" +
		"\t/usr/lib/libresolv.9.dylib (compatibility version 1.0.0, current version 1.0.0)\n" +
		"\t/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation (compatibility version 150.0.0, current version 2420.0.0)\n"
	if ok, off := isCgoFreeLinkage(clean); !ok || len(off) != 0 {
		t.Errorf("clean darwin linkage: ok=%v offending=%v, want ok=true offending=[]", ok, off)
	}

	// A bundled libsqlite3 (what a cgo SQLite drags in) MUST be flagged.
	dirty := "/tmp/mad-substrate:\n" +
		"\t/usr/lib/libSystem.B.dylib (compatibility version 1.0.0, current version 1.0.0)\n" +
		"\t/opt/homebrew/opt/sqlite/lib/libsqlite3.0.dylib (compatibility version 9.0.0, current version 9.6.0)\n"
	ok, off := isCgoFreeLinkage(dirty)
	if ok {
		t.Errorf("dirty darwin linkage reported cgo-free; want flagged. offending=%v", off)
	}
	if !containsSubstr(off, "libsqlite3") {
		t.Errorf("offending=%v, want it to name the bundled libsqlite3", off)
	}

	// Non-vacuity: empty/garbage otool output must NOT read as clean.
	if ok, _ := isCgoFreeLinkage(""); ok {
		t.Errorf("empty otool output reported cgo-free; the scan must be non-vacuous (ok=false)")
	}
}

func TestIsSystemSharedObject_Linux(t *testing.T) {
	cases := []struct {
		lib  string
		want bool
	}{
		{"linux-vdso.so.1", true},
		{"/lib64/ld-linux-x86-64.so.2", true},
		{"ld-musl-x86_64.so.1", true},
		{"libc.so.6", true},
		{"/lib/x86_64-linux-gnu/libc.so.6", true},
		{"libpthread.so.0", true},
		{"libdl.so.2", true},
		{"libm.so.6", true},
		{"libresolv.so.2", true},
		{"libnss_files.so.2", true},
		// Third-party / bundled C — must be flagged.
		{"libsqlite3.so.0", false},
		{"/opt/lib/libstdc++.so.6", false},
		{"libcurl.so.4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isSystemSharedObjectLinux(c.lib); got != c.want {
			t.Errorf("isSystemSharedObjectLinux(%q) = %v, want %v", c.lib, got, c.want)
		}
	}
}

func TestIsCgoFreeLinkage_Linux(t *testing.T) {
	// (1) ldd: a fully static cgo-free Go binary.
	if ok, off := isCgoFreeLinkageLinux("\tnot a dynamic executable\n", false); !ok || len(off) != 0 {
		t.Errorf("ldd static binary: ok=%v offending=%v, want ok=true offending=[]", ok, off)
	}

	// (2) ldd: a dynamic glibc binary linking only the system set — clean.
	lddSystem := "\tlinux-vdso.so.1 (0x00007fffe5b000)\n" +
		"\tlibc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x00007f8a1c000000)\n" +
		"\t/lib64/ld-linux-x86-64.so.2 (0x00007f8a1c400000)\n"
	if ok, off := isCgoFreeLinkageLinux(lddSystem, false); !ok || len(off) != 0 {
		t.Errorf("ldd system-only: ok=%v offending=%v, want ok=true offending=[]", ok, off)
	}

	// (3) ldd: a bundled libsqlite3 MUST be flagged.
	lddDirty := "\tlinux-vdso.so.1 (0x00007fffe5b000)\n" +
		"\tlibc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x00007f8a1c000000)\n" +
		"\tlibsqlite3.so.0 => /usr/lib/libsqlite3.so.0 (0x00007f8a1c800000)\n"
	ok, off := isCgoFreeLinkageLinux(lddDirty, false)
	if ok {
		t.Errorf("ldd with libsqlite3 reported cgo-free; want flagged. offending=%v", off)
	}
	if !containsSubstr(off, "libsqlite3") {
		t.Errorf("offending=%v, want it to name libsqlite3", off)
	}

	// (4) readelf -d: NEEDED only the system set — clean.
	readelfClean := "Dynamic section at offset 0x2d40 contains 5 entries:\n" +
		"  Tag        Type                         Name/Value\n" +
		" 0x0000000000000001 (NEEDED)             Shared library: [libc.so.6]\n"
	if ok, off := isCgoFreeLinkageLinux(readelfClean, true); !ok || len(off) != 0 {
		t.Errorf("readelf system-only: ok=%v offending=%v, want ok=true offending=[]", ok, off)
	}

	// (5) readelf -d: NEEDED a bundled libsqlite3 MUST be flagged.
	readelfDirty := " 0x0000000000000001 (NEEDED)             Shared library: [libc.so.6]\n" +
		" 0x0000000000000001 (NEEDED)             Shared library: [libsqlite3.so.0]\n"
	if ok, off := isCgoFreeLinkageLinux(readelfDirty, true); ok || !containsSubstr(off, "libsqlite3") {
		t.Errorf("readelf with libsqlite3: ok=%v offending=%v, want ok=false naming libsqlite3", ok, off)
	}

	// (6) readelf -d: no NEEDED entries (fully static) — clean.
	readelfStatic := "There is no dynamic section in this file.\n"
	if ok, off := isCgoFreeLinkageLinux(readelfStatic, true); !ok || len(off) != 0 {
		t.Errorf("readelf static binary: ok=%v offending=%v, want ok=true offending=[]", ok, off)
	}
}

// containsSubstr reports whether any element of xs contains sub.
func containsSubstr(xs []string, sub string) bool {
	for _, x := range xs {
		if len(sub) > 0 && len(x) >= len(sub) && indexOf(x, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
