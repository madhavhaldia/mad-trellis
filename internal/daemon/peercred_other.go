//go:build !darwin && !linux

package daemon

// peerUID has no portable peer-credential primitive on this platform, so it
// always reports ok=false and the caller fails OPEN (the file-perm gate remains
// the control). This keeps the daemon cgo-free and buildable everywhere.
func peerUID(fd uintptr) (uint32, bool) {
	return 0, false
}
