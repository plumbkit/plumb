package cli

// config_watcher_fsnotify.go — hardening shared by the global and project
// config watchers against a descriptor race in fsnotify's kqueue backend
// (macOS and the BSDs).
//
// Since fsnotify v1.10.0 (#740), kqueue's Watcher.Close closes every watch
// descriptor itself, but nothing stops its reader goroutine doing the same
// thing at the same time: when a watched path is deleted or renamed the
// reader drops that watch and closes its descriptor too. Close and the reader
// both look the watch up before either removes it, so both close the same
// number. If anything else in the process opens a file in between, the second
// close lands on that file instead. In plumb's test suite the casualty was the
// next test's freshly made watcher (its kqueue, or one of its watch
// descriptors) or the next temp-dir clean-up; in a daemon it could be any
// descriptor. The double close itself is fsnotify's to fix. Plumb does two
// things around it: closing a watcher waits until the watcher is completely
// closed (closeFSWatcher, and the project-watch manager's close), so a caller
// that then deletes the watched tree — a test's temp-dir clean-up — cannot
// race the close; and a watcher that lost its own descriptor is recreated
// rather than left dead.

import (
	"errors"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fsWatcherDrainTimeout bounds how long closeFSWatcher waits for a watcher's
// reader goroutine to exit. Every backend's reader exits promptly once Close
// has run; the bound only stops a backend bug from hanging daemon shutdown.
const fsWatcherDrainTimeout = 5 * time.Second

// watcherRecreateInterval is the least time between two in-place recreations
// of one lost watcher. Losing the replacement inside it means the cause is
// persistent rather than one stolen descriptor, so the caller stops retrying.
const watcherRecreateInterval = time.Second

// closeFSWatcher closes watcher and waits for its reader goroutine to exit,
// which every backend signals by closing Events. kqueue's Close returns
// without waiting for that reader, which may still be closing a watch
// descriptor; waiting here means that once this returns, nothing of this
// watcher is still open or still being closed. A nil watcher is a no-op.
func closeFSWatcher(watcher *fsnotify.Watcher) {
	if watcher == nil {
		return
	}
	_ = watcher.Close()
	deadline := time.NewTimer(fsWatcherDrainTimeout)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-watcher.Events:
			if !ok {
				return
			}
		case <-deadline.C:
			return
		}
	}
}

// fsWatcherLost reports whether err means the watcher lost one of its own
// descriptors (EBADF): the kqueue reader cannot read its queue again
// ("fsnotify.readEvents: bad file descriptor", repeated without end), and an
// Add that hit it fails the same way. A watcher in that state never delivers
// another event; only a fresh watcher recovers.
func fsWatcherLost(err error) bool {
	return errors.Is(err, syscall.EBADF)
}
