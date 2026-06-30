//go:build linux

package daemon

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// peerUID reads the connected peer's uid on Linux via the SO_PEERCRED ucred at
// SOL_SOCKET. Any error yields ok=false so the caller fails OPEN.
func peerUID(fd uintptr) (uint32, bool) {
	uc, err := unix.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil || uc == nil {
		return 0, false
	}
	return uc.Uid, true
}
