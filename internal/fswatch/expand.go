package fswatch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// maxExpand bounds how many entries one directory that arrived whole may
// report. A bigger tree is left to a full reconcile, which Lost requests.
const maxExpand = 10000

// errExpandLimit stops an expansion walk at the Watcher's expandLimit.
var errExpandLimit = errors.New("fswatch: expansion limit")

// excluded applies Options.ExcludeRegex as sgtdi does: to the full path and to
// the base name.
func (w *Watcher) excluded(path string) bool {
	return w.exclude != nil && (w.exclude.MatchString(path) || w.exclude.MatchString(filepath.Base(path)))
}

// expandIfDir reports, through emit, everything inside path when an event says
// path was created or renamed into place and it is a directory.
//
// Neither backend reports the contents of a directory that arrives whole.
// FSEvents reports a populated directory moved into the tree as that one
// directory. inotify reports a new directory, but files created inside it
// before the backend has registered a watch on it are never reported at all
// (`mkdir -p a/b/c && touch a/b/c/x` yields only `a`). Consumers that index
// files (the topology index skips directories) would miss them until a full
// resync. Anything changed inside the directory after its watch is live is
// reported separately as well, so a file may be reported twice; consumers are
// idempotent.
//
// The walk opens directories and never a regular file, so it cannot disturb a
// SQLite lock; it does not follow symlinks; it prunes excluded directories.
// When it cannot report everything (past expandLimit entries, or a
// subdirectory it cannot read) it asks for a reconcile. An entry that vanished
// mid-walk is not a loss: its removal is an event of its own.
func (w *Watcher) expandIfDir(path string, op Op, emit func(string)) {
	if arrivedDir(path, op) {
		w.expand(path, emit)
	}
}

// arrivedDir reports whether an event says path was created or renamed into
// place, and it is a directory.
func arrivedDir(path string, op Op) bool {
	if !op.Has(Create | Rename) {
		return false
	}
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}

// expand reports everything inside the directory path through emit; see
// expandIfDir.
func (w *Watcher) expand(path string, emit func(string)) {
	n := 0
	incomplete := false
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				incomplete = true
			}
			return nil // skip this entry and carry on with its siblings
		}
		if p == path {
			return nil
		}
		if d.IsDir() {
			// With the trailing separator, a pattern that excludes everything
			// UNDER a directory (ExcludeDirsRegex's dot branch) prunes it whole.
			if w.excluded(p) || w.excluded(p+string(filepath.Separator)) {
				return filepath.SkipDir
			}
		} else if w.excluded(p) {
			return nil
		}
		if n++; n > w.expandLimit {
			return errExpandLimit
		}
		emit(p)
		return nil
	})
	if incomplete || errors.Is(err, errExpandLimit) {
		w.signalLost()
	}
}
