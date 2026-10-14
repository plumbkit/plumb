//go:build linux

package cli

import (
	"os"
	"syscall"
)

// identityOf returns the identity of info on Linux: device, inode and change
// time.
//
// The change time is Ctim here and Ctimespec on darwin — the field NAME differs
// per platform even though the value means the same thing — which is why this is
// a per-platform file rather than one function with a cast inside it.
func identityOf(info os.FileInfo) fileIdentity {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}
	}
	return fileIdentity{dev: st.Dev, ino: st.Ino, ctime: timespecNanos(st.Ctim)}
}

// timespecNanos renders a kernel timestamp as nanoseconds since the epoch.
func timespecNanos(ts syscall.Timespec) int64 {
	return ts.Sec*1e9 + ts.Nsec
}
