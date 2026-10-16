//go:build linux

package fswatch

import (
	"os"

	"golang.org/x/sys/unix"
)

// birthTime is the directory's birth time from statx(2), which a filesystem
// reports only when it records one (ext4, xfs, btrfs and tmpfs do).
func birthTime(path string, _ os.FileInfo) (int64, bool) {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stx); err != nil {
		return 0, false
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return 0, false
	}
	return stx.Btime.Sec*1e9 + int64(stx.Btime.Nsec), true
}
