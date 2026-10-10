//go:build linux

package fswatch

import (
	"errors"

	"golang.org/x/sys/unix"
)

// inotifyLimitReached reports whether the inotify watch limit is exhausted,
// by adding a watch on dir through a throwaway inotify instance. Closing the
// instance drops the watch again, so the probe holds nothing. It reports false
// when it cannot tell (no instance could be made), so only a definite ENOSPC
// degrades a Watcher.
func inotifyLimitReached(dir string) bool {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(fd) }()
	_, err = unix.InotifyAddWatch(fd, dir, unix.IN_DELETE_SELF)
	return errors.Is(err, unix.ENOSPC)
}
