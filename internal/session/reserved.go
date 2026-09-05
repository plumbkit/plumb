package session

// reserved.go — name reservations held by sessions that are not live.
//
// Session-name uniqueness has always been checked against the LIVE session
// directory, which is exactly right while every session that owns a name is
// running. It stops being right the moment a `plumb serve` outlives its daemon:
// the daemon restarts, every connSession is destroyed, and for the length of
// the outage the surviving proxy's name belongs to nobody. The next session to
// draw a name can take it, and when the survivor reconnects the name it has
// been addressed by all conversation is gone — it is renamed by the collision
// path and every note written to the old name is orphaned.
//
// A reservation closes that window. It is supplied by the caller rather than
// read here, because the durable identity records live in internal/sessionstate
// and this package must not depend on them: internal/cli owns both and passes
// the map down. That also keeps every reservation decision testable with a
// literal.

import "strings"

// Reserved maps a LOWER-CASED session name to the plumb session ID entitled to
// it — a name held by a recoverable-but-not-live identity.
//
// Lower-cased because nameTaken compares case-insensitively; keeping the two
// consistent means a reservation can only refuse a confusable name, never admit
// one.
//
// The value matters as much as the key. A session may always take a name
// reserved for ITSELF — that is the whole point of the reservation — so a
// reservation whose value is this session's ID is not a collision. A reservation
// with an empty value would be a name nobody could ever claim, which is why the
// store omits those rows.
type Reserved map[string]string

// taken reports whether name is reserved for a session other than selfID.
// A nil or empty Reserved answers false for everything, so every existing call
// path keeps its exact previous behaviour.
func (r Reserved) taken(name, selfID string) bool {
	if len(r) == 0 || name == "" {
		return false
	}
	owner, ok := r[strings.ToLower(name)]
	return ok && owner != selfID
}

// RegisterReserved is Register with a set of names reserved for absent sessions.
// Register itself is RegisterReserved(info, nil), so the two cannot drift.
//
// A caller-supplied name that is reserved for another session is refused with
// ErrNameTaken, exactly as a name held by a live session is: the caller asked
// for that particular name, and silently substituting another is how a rename
// becomes invisible. A GENERATED name simply redraws.
func RegisterReserved(info Info, reserved Reserved) (Info, error) {
	return register(info, reserved)
}

// RenameReserved is Rename with a set of names reserved for absent sessions.
// Rename itself is RenameReserved(id, name, nil).
//
// A reservation for THIS session is not a collision, so a reconnecting session
// restoring its own retained name always succeeds against its own reservation —
// which is the case the reservation exists to serve.
func RenameReserved(id, name string, reserved Reserved) (string, error) {
	return rename(id, name, reserved)
}
