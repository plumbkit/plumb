package session

// list_cache.go remembers which session files record an ENDED session, so List
// does not re-open and re-parse them on every call.
//
// Why it exists (#545): ended sessions are kept for endedSessionGrace so a
// reconnecting agent can inherit its name, and a machine running several agents
// accumulates about a thousand of them a day. List opened every one, under the
// session directory's EXCLUSIVE flock, on every call. Opening a file costs about
// 0.25 ms on macOS (a stat costs a hundredth of that), so each List took ~290 ms
// and every caller queued behind the one before: two concurrent
// workspace_sessions calls blew its 500 ms budget with "timed out reading
// session or stats data". Raising the budget would only have moved the cliff.
//
// An ended file does not change while it waits out its grace, so what List needs
// from it — "ended, at this time" — can be kept, keyed on the file's identity.
// Every writer replaces a session file by atomic rename (a new inode) or sets
// its mtime (Touch), so an unchanged (inode, size, mtime) is an unchanged file;
// anything else is a miss and the file is read as before. Only ended files are
// remembered: a live session's record is always read fresh.

import (
	"os"
	"sync"
	"time"
)

// readSessionFile reads one session file for listLocked. A variable only so a
// test can count reads (export_test.go).
var readSessionFile = os.ReadFile

// endedFile is what List keeps about one ended session's file.
type endedFile struct {
	fi      os.FileInfo // the file as it was when read: its identity
	endedAt time.Time
}

// endedFileCache holds the ended files of one session directory.
//
// Concurrency: guarded by mu. Callers also hold the session-directory flock, but
// that orders processes, not this process's memory, so mu is still required.
type endedFileCache struct {
	mu    sync.Mutex
	dir   string
	files map[string]endedFile // by file name within dir
}

// endedFiles is process-global because List is: every caller in the daemon lists
// the same directory, and the saving comes from sharing what one call learnt.
// Like stats.SharedReadOnly it is a cache, never a source of truth — a miss only
// costs the read List always did.
var endedFiles endedFileCache

// lookup returns when the ended session in dir/name ended, if the file is still
// the one that was read.
func (c *endedFileCache) lookup(dir, name string, fi os.FileInfo) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir != dir {
		return time.Time{}, false
	}
	e, ok := c.files[name]
	if !ok || !os.SameFile(e.fi, fi) || e.fi.Size() != fi.Size() || !e.fi.ModTime().Equal(fi.ModTime()) {
		return time.Time{}, false
	}
	return e.endedAt, true
}

// retain replaces dir's entries with kept: the ended files one full scan saw.
// Replacing rather than merging drops files that were deleted or revived.
func (c *endedFileCache) retain(dir string, kept map[string]endedFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dir, c.files = dir, kept
}

// pruneOrKeepEnded handles an ended session's file for listLocked: removed once
// its grace has passed, otherwise remembered in kept (when its identity is known)
// so the next List need not read it again.
func pruneOrKeepEnded(path, name string, ef endedFile, kept map[string]endedFile) {
	if time.Since(ef.endedAt) > endedSessionGrace {
		_ = os.Remove(path)
		return
	}
	if ef.fi != nil {
		kept[name] = ef
	}
}
