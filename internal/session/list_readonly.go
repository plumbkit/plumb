package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// list_readonly.go is the registry's way in for a process that only READS it and
// may run where the session-directory lock cannot be opened at all (PLAN-495).
//
// The lock needs an O_CREATE|O_RDWR open of .sessions.lock. A sandbox that denies
// writes under the data directory answers that open with "operation not
// permitted" while the directory itself still lists fine — so `plumb mail` and
// the lifecycle hooks, which only want to know who is live, failed outright in
// such a harness although every byte they need was readable.

// ListForReading returns the live sessions and whether they were read under the
// session-directory lock.
//
// When the lock can be taken this IS List, side effects included (a dead PID is
// marked ended, expired ended files are pruned). When the lock cannot even be
// opened — a permission or read-only-filesystem error, never contention, since
// flock blocks rather than failing — it falls back to listReadOnly, and locked is
// false so the caller can say its answer was read without the lock.
//
// Its one caller today is `plumb mail` (internal/cli/mail.go,
// resolveMailSessionReading), which reports locked=false as "unlocked_read". A new
// caller must surface locked=false the same way rather than discard it: an answer
// read without the lock must not be presented as an authoritative one.
func ListForReading() (infos []Info, locked bool, err error) {
	dir, err := Dir()
	if err != nil {
		return nil, false, err
	}
	err = withSessionDirLock(dir, func() error {
		var lerr error
		infos, lerr = listLocked(dir)
		return lerr
	})
	if err == nil {
		return infos, true, nil
	}
	if !lockUnavailable(err) {
		return nil, false, err
	}
	infos, err = listReadOnly(dir)
	return infos, false, err
}

// lockUnavailable reports whether taking the session-directory lock failed
// because this process may not open or create the lock file at all, as opposed
// to an ordinary I/O failure that should still be reported.
func lockUnavailable(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// listReadOnly is listLocked without a single write: a session file whose PID is
// dead is skipped rather than marked ended, and nothing is pruned, because a
// process that may not take the lock must not mutate what the lock protects.
//
// Reading without the lock cannot tear a file — every session file is written by
// temp file plus rename (writeSessionFileAtomic), so it is read whole or not at
// all. What it can do is race a session that is starting or ending at this
// instant, and miss it or still list it; that is the cost callers are told about.
//
// An unreadable or unparsable file is skipped without a count, exactly as
// listLocked skips it. With atomic writes such a file is not a torn write in
// progress but a damaged one, which the locked path drops just as silently, so a
// count here would describe the fallback as less reliable than it is.
func listReadOnly(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading session dir: %w", err)
	}
	var infos []Info
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := readSessionFile(path)
		if err != nil {
			continue
		}
		var info Info
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		if !info.EndedAt.IsZero() || !pidAlive(info.PID) {
			continue
		}
		if fi, err := os.Stat(path); err == nil {
			info.LastSeenAt = fi.ModTime()
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].StartedAt.Before(infos[j].StartedAt)
	})
	return infos, nil
}
