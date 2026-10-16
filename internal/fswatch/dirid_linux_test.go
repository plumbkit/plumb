//go:build linux

package fswatch

import "golang.org/x/sys/unix"

// platformRecordsBirth reports, independently of dirID's wiring, whether this
// filesystem records a birth time for path.
func platformRecordsBirth(path string) bool {
	var stx unix.Statx_t
	return unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stx) == nil && stx.Mask&unix.STATX_BTIME != 0
}
