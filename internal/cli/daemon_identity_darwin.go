//go:build darwin

package cli

import (
	"os"
	"syscall"
)

// identityOf returns the identity of info on darwin: device, inode and change
// time (Ctimespec here, Ctim on Linux — the field name is the only difference).
func identityOf(info os.FileInfo) fileIdentity {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}
	}
	//nolint:gosec // G115: darwin's Dev is an int32 device number, an opaque kernel identifier here and never arithmetic.
	return fileIdentity{dev: uint64(st.Dev), ino: st.Ino, ctime: timespecNanos(st.Ctimespec)}
}

// timespecNanos renders a kernel timestamp as nanoseconds since the epoch.
func timespecNanos(ts syscall.Timespec) int64 {
	return ts.Sec*1e9 + ts.Nsec
}
