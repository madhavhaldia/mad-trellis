//go:build !coopembed

package coopembed

import "testing"

// TestStubReportsNoAssets pins the //go:build !coopembed STUB face: the default
// (untagged) build — which is the build the test suite, `go vet`, and the critical
// `CGO_ENABLED=0 go build ./...` CI invariant all compile — embeds NOTHING. Every
// accessor must report "absent" rather than fail.
//
// NON-VACUOUS: the assertions would FLIP if the stub ever returned bytes or
// Available()==true — i.e. if the tagged embed face leaked into an untagged build.
// (The real face is exercised by the tagged release build + the launcher live e2e.)
func TestStubReportsNoAssets(t *testing.T) {
	if Available() {
		t.Fatal("stub (untagged) build must report Available()==false")
	}
	for _, arch := range []string{"arm64", "amd64", "riscv64"} {
		if b, ok := RelayBytes(arch); ok || b != nil {
			t.Fatalf("stub RelayBytes(%q) = (%v, %v); want (nil, false)", arch, b, ok)
		}
		if b, ok := MadTrellisBytes(arch); ok || b != nil {
			t.Fatalf("stub MadTrellisBytes(%q) = (%v, %v); want (nil, false)", arch, b, ok)
		}
	}
}
