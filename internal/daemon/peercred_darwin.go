//go:build darwin

package daemon

import "golang.org/x/sys/unix"

// peerUID reads the connected peer's uid on macOS via the LOCAL_PEERCRED
// xucred at SOL_LOCAL. Any error yields ok=false so the caller fails OPEN.
func peerUID(fd uintptr) (uint32, bool) {
	xc, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil || xc == nil {
		return 0, false
	}
	return xc.Uid, true
}
