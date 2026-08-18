package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrIDTaken is returned by Adopt when another LIVE session already holds the
// requested session ID. Match it with errors.Is.
var ErrIDTaken = errors.New("session ID is already in use by a live session")

// Adopt re-registers the session identified by oldID under newID, preserving
// every field of its record except the ID and EndedAt, and marks the old record
// ended. It is the daemon's session-ID adoption path (PLAN-296): a reconnecting
// connection presents the stable session ID the serve proxy replayed, and the
// daemon adopts it so stats, memories and collab see one continuous identity
// across the restart.
//
// Refuses ErrIDTaken when another LIVE session already holds newID — the
// overlap case, where the previous daemon is still running and its session
// still answers to that ID. The caller keeps its generated ID and retries on
// the next reconnect. oldID == newID returns the existing record unchanged.
//
// The read-modify-write runs under the session-directory flock, so the "is
// newID free?" check and both writes cannot interleave with a concurrent
// Register or Rename in this or another process. The old record is ended before
// the new one is written, so the name it holds is released to the new record
// under the same lock and a concurrent reader never observes both live.
func Adopt(oldID, newID string) (Info, error) {
	if oldID == newID {
		return readInfo(oldID)
	}
	dir, err := Dir()
	if err != nil {
		return Info{}, err
	}
	var out Info
	if err := withSessionDirLock(dir, func() error {
		live, err := listLocked(dir)
		if err != nil {
			return err
		}
		if idTaken(live, newID) {
			return fmt.Errorf("%w: %q", ErrIDTaken, newID)
		}
		info, err := readInfo(oldID)
		if err != nil {
			return err
		}
		info.EndedAt = time.Now()
		if err := writeSessionFileAtomic(filepath.Join(dir, oldID+".json"), info); err != nil {
			return fmt.Errorf("ending session file: %w", err)
		}
		info.ID = newID
		info.EndedAt = time.Time{}
		if err := writeSessionFileAtomic(filepath.Join(dir, newID+".json"), info); err != nil {
			return fmt.Errorf("writing session file: %w", err)
		}
		out = info
		return nil
	}); err != nil {
		if errors.Is(err, ErrIDTaken) {
			return Info{}, err
		}
		return Info{}, fmt.Errorf("adopting session ID: %w", err)
	}
	return out, nil
}

// idTaken reports whether a live session holds id.
func idTaken(live []Info, id string) bool {
	for _, info := range live {
		if info.ID == id {
			return true
		}
	}
	return false
}

// readInfo reads and decodes the session record at id. It does no locking:
// Adopt calls it inside the session-directory flock (after listLocked), or on
// the oldID == newID short-circuit where no write is made and a torn read is
// the same class of harmless race List already tolerates.
func readInfo(id string) (Info, error) {
	dir, err := Dir()
	if err != nil {
		return Info{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return Info{}, fmt.Errorf("reading session file: %w", err)
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, fmt.Errorf("decoding session file: %w", err)
	}
	return info, nil
}
