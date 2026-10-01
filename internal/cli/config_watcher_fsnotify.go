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
// descriptor. The double close itself is fsnotify's to fix, and only an
// upstream fix removes it. Plumb narrows the window in two ways. Closing a
// watcher waits until its reader has stopped delivering (closeFSWatcher, and
// the project-watch manager's close), so a caller that then deletes the
// watched tree — a test's temp-dir clean-up — no longer has Close and the
// reader both reacting to that delete. And a watcher that lost its own
// descriptor is recreated rather than left dead. The wait cannot cover the
// reader's last two closes (its kqueue and close pipe), which come just after
// it closes Events and so can trail the wait by microseconds.

import (
	"errors"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fsWatcherDrainTimeout bounds how long closeFSWatcher waits for a watcher's
// reader goroutine to stop delivering. Every backend's reader stops promptly
// once Close has run; the bound only stops a backend bug from hanging a close.
const fsWatcherDrainTimeout = 5 * time.Second

// watcherRecreateInterval is the least time between two in-place recreations
// of one lost watcher. Losing the replacement inside it means the cause is
// persistent rather than one stolen descriptor, so the caller stops retrying.
const watcherRecreateInterval = time.Second

// closeFSWatcher closes watcher and waits until its reader goroutine has
// stopped delivering, which every backend signals by closing Events. kqueue's
// Close returns without waiting for that reader, which may still be handling
// an event, and so dropping a watch and closing its descriptor. Once this
// returns the reader is past its last event, so a path deleted from here on
// cannot make it close a descriptor that Close also closed.
//
// It does not wait for the reader's final clean-up: kqueue's reader closes
// Events and Errors first and its own kqueue and close-pipe descriptors just
// after, so those two closes can still trail this return by microseconds. For
// a healthy watcher that is harmless; for one that already lost its kqueue
// descriptor (the recreate paths) the trailing close can land on whatever has
// reused the number. So this shrinks the window rather than closing it: only a
// fix in fsnotify removes it. A nil watcher is a no-op.
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
