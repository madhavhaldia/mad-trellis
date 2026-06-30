//go:build coopembed

package coopembed

import (
	"embed"
	"fmt"
)

// assets carries the cross-built static linux cooperative-plane payloads. The
// Makefile `build` target writes mad-substrate-relay-linux-<arch> and
// mad-substrate-linux-<arch> here BEFORE the -tags coopembed darwin build, then embeds
// them. The dir always contains .gitkeep so this //go:embed never fails on a clean
// tree; the binaries themselves are .gitignored (never committed).
//
//go:embed assets
var assets embed.FS

// Available reports whether the embed face is compiled in — always true here. (A
// specific arch's bytes may still be absent; probe with RelayBytes/MadSubstrateBytes.)
func Available() bool { return true }

// RelayBytes returns the embedded static linux relay binary for goarch, or
// (nil, false) when that arch's asset is absent or empty.
func RelayBytes(goarch string) ([]byte, bool) {
	return readAsset(fmt.Sprintf("assets/mad-substrate-relay-linux-%s", goarch))
}

// MadSubstrateBytes returns the embedded static linux mad-substrate binary for goarch, or
// (nil, false) when that arch's asset is absent or empty.
func MadSubstrateBytes(goarch string) ([]byte, bool) {
	return readAsset(fmt.Sprintf("assets/mad-substrate-linux-%s", goarch))
}

// readAsset reads one embedded payload, treating a missing OR empty file as absent
// (fail-soft) so a partial cross-build never hands the caller a zero-byte binary.
func readAsset(name string) ([]byte, bool) {
	b, err := assets.ReadFile(name)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}
