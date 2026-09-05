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
// every field of the ADOPTER's record except the ID and EndedAt, and marks the
// old record ended. When the adopter has no ExternalID, it carries that field
// forward from the predecessor record at newID so external session linkage
// survives a daemon restart (PLAN-404). It is the daemon's session-ID adoption
// path (PLAN-296): a reconnecting
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
	return AdoptWithExternalID(oldID, newID, "")
}

// AdoptWithExternalID is Adopt with a fallback external-conversation linkage,
// used when the predecessor's session JSON is no longer on disk.
//
// Adopt's carry-forward reads the record at newID, which the session-directory
// janitor deletes 24 h after that session ended. So an outage longer than the
// grace window recovered the ID and the name — both held durably — and silently
// dropped the linkage, leaving `plumb mail --external-id` unable to resolve a
// session that had in fact recovered perfectly. fallbackExternalID is the value
// from the canonical durable identity record, which does not expire.
//
// Precedence is deliberate and narrow. The adopter's own ExternalID wins if it
// has one (it ran session_start and declared itself). The predecessor JSON wins
// next, because it is the record the previous daemon actually wrote. The
// fallback applies only when neither is available — it fills a gap, it never
// overwrites live evidence. An empty fallback carries nothing, exactly as
// before: an absent record is no authority to invent an identity.
func AdoptWithExternalID(oldID, newID, fallbackExternalID string) (Info, error) {
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
		// The predecessor is an ended file at newID. Its ExternalID was linked by
		// session_start before the restart, while this fresh adopter has not had a
		// chance to run session_start yet. Failure to read it carries nothing: an
		// absent or corrupt predecessor is no authority to invent an identity.
		if predecessor, err := readInfo(newID); err == nil && info.ExternalID == "" {
			info.ExternalID = predecessor.ExternalID
		}
		if info.ExternalID == "" {
			info.ExternalID = fallbackExternalID
		}
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
