package sessionstate

// identity.go — the CANONICAL durable identity record.
//
// Every other table here is a cache: a read record saves a re-read, a pin saves
// a re-declaration, and losing either costs one round-trip. This one is not. It
// is the only durable answer to "which plumb session is this reconnecting
// `plumb serve`?", and it is keyed by the proxy session ID — a 122-bit secret
// the serve process generates for itself and never discloses. That key is what
// makes the record an authorisation and not merely a hint: presenting it is
// evidence of being the same serve process, whereas presenting a session's
// public ID or name is evidence of nothing at all (both are echoed to clients).
//
// Because it is the authority, three rules hold here and nowhere else in this
// package:
//
//   - It is never expired by age (see Prune). A serve process that has been
//     connected for a week is exactly the one that most needs its row.
//   - A blank EXTERNAL LINKAGE never overwrites a known one. A caller that has
//     not learned it yet must not be able to erase the linkage the record
//     already proves. (Name and SessionID are replaced outright — the live
//     session is authoritative for those — so the guard is deliberately
//     narrow, and it is the CALLER's job not to offer a temporary identity;
//     see renameSessionClaiming.)
//   - Its name is RESERVED while the identity is recoverable, so a new session
//     cannot draw the name a disconnected one will come back to.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Identity is the durable record of who a proxy session is: the display name it
// answers to, the plumb session ID that name belongs to, the authorised
// external-conversation linkage, and the revision that orders name changes.
//
// Name and SessionID travel together because a reconnect needs both. The name is
// what peers address; the session ID is what a message addressed to that name is
// BOUND to, so a reconnecting proxy has to resume its predecessor's ID to
// collect mail written before the restart. Storing the ID beside the name is
// what makes that provable rather than assumed from the name alone.
//
// ExternalID joined them in PLAN-426. It used to be recoverable only from the
// predecessor's session JSON file, which is garbage collected 24 h after that
// session ends — so an outage longer than the grace window dropped the linkage
// while the identity itself survived, and `plumb mail --external-id` stopped
// resolving a session that was still very much alive.
type Identity struct {
	// Name is the session's display name.
	Name string
	// SessionID is the plumb session ID that held Name. Empty on a row written
	// before that column existed (schema v3), which proves no predecessor.
	SessionID string
	// ExternalID is the caller's own conversation ID, as linked by
	// session_start. Empty means UNKNOWN — never "this session has none" — so a
	// caller must not treat it as authority to clear a linkage.
	ExternalID string
	// NameRevision increments on every recorded name CHANGE. It orders updates
	// so a snapshot taken before an explicit rename cannot be replayed over the
	// newer name. 0 on a row that predates the column (schema v7).
	NameRevision int64
}

// SaveIdentity records a session's durable identity under its proxy session ID,
// so a reconnect after a daemon restart comes back as the same session. nil-safe;
// a no-op when proxySessionID or id.Name is empty.
//
// Merge semantics, which are the whole point:
//
//   - Name and SessionID are replaced. They are what the live session actually
//     holds, and the live session is authoritative for its own identity.
//   - ExternalID is replaced only by a NON-EMPTY value. A caller that has not
//     yet learned the linkage passes "" and must not thereby erase the linkage
//     the record already proves — that is how PLAN-404's carry-forward used to
//     be lost on the first save after a reconnect.
//   - NameRevision auto-increments when, and only when, the name actually
//     changes. A save that re-records the same name (the common case: every
//     reconnect refreshes the row) leaves the revision alone, so a revision
//     comparison means "the name moved", not "something was written".
func (s *Store) SaveIdentity(proxySessionID string, id Identity) error {
	if s == nil || proxySessionID == "" || id.Name == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO session_names (proxy_session_id, name, plumb_session_id, external_id, name_revision, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?)
		 ON CONFLICT(proxy_session_id)
		 DO UPDATE SET name=excluded.name,
		               plumb_session_id=excluded.plumb_session_id,
		               external_id=CASE WHEN excluded.external_id != '' THEN excluded.external_id
		                                ELSE session_names.external_id END,
		               name_revision=CASE WHEN session_names.name != excluded.name
		                                  THEN session_names.name_revision + 1
		                                  ELSE session_names.name_revision END,
		               updated_at=excluded.updated_at`,
		proxySessionID, id.Name, id.SessionID, id.ExternalID, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sessionstate: save identity: %w", err)
	}
	return nil
}

// LoadIdentity returns the identity recorded under proxySessionID. ok is false
// when none is recorded. nil-safe (returns ok=false).
//
// A row written before a given column existed reads that column's zero value, so
// an upgraded daemon degrades to exactly the behaviour that shipped before it:
// an empty SessionID proves no predecessor, an empty ExternalID means the
// linkage is unknown, and revision 0 sorts below every recorded change. None of
// them is ever a wildcard.
func (s *Store) LoadIdentity(proxySessionID string) (id Identity, ok bool, err error) {
	if s == nil || proxySessionID == "" {
		return Identity{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(
		`SELECT name, plumb_session_id, external_id, name_revision
		   FROM session_names WHERE proxy_session_id=?`,
		proxySessionID,
	)
	switch err := row.Scan(&id.Name, &id.SessionID, &id.ExternalID, &id.NameRevision); err {
	case nil:
		return id, true, nil
	case sql.ErrNoRows:
		return Identity{}, false, nil
	default:
		return Identity{}, false, fmt.Errorf("sessionstate: load identity: %w", err)
	}
}

// Reservation is one retained identity's claim on a name: the name itself, the
// plumb session ID that holds it, and the external conversation it is linked to.
//
// The external ID is here because the session ID is not always enough to decide
// entitlement. A `plumb serve` that RESTARTS gets a new proxy secret and a new
// internal session ID, so it can present neither — yet if it re-links the same
// conversation it is the same agent, and the name is being held for exactly it.
// Without this field the reservation would lock the name away from the only
// party entitled to it.
type Reservation struct {
	// Name is the reserved name, as stored (compare case-insensitively).
	Name string
	// SessionID is the plumb session ID holding it. Never empty — a row with no
	// session ID reserves nothing (see Reservations).
	SessionID string
	// ExternalID is the conversation the holder was linked to, or "" when
	// unknown. Empty never matches, so it can only ever widen entitlement to a
	// caller that names the same conversation, never to one that names none.
	ExternalID string
}

// Reservations returns every name a recoverable identity holds. nil-safe.
//
// This is the reservation authority the session directory alone cannot provide.
// Session-name uniqueness has always been checked against LIVE sessions, which
// is correct while every session that owns a name is running — but a `plumb
// serve` that outlives its daemon has no live record at all, and its name would
// be handed to the next session to draw one. It then comes back to find its own
// name taken, is renamed by the collision path, and every note addressed to it
// is orphaned. Reserving here closes that window for as long as the identity
// stays recoverable.
//
// Rows with no plumb_session_id are skipped: such a row reserves a name that no
// session could ever claim as its own, which locks the name out permanently
// rather than holding it for someone.
func (s *Store) Reservations() ([]Reservation, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT name, plumb_session_id, external_id FROM session_names WHERE plumb_session_id != ''`,
	)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: reservations: %w", err)
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.Name, &r.SessionID, &r.ExternalID); err != nil {
			return nil, fmt.Errorf("sessionstate: scan reservation: %w", err)
		}
		if r.Name == "" {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessionstate: reservations: %w", err)
	}
	return out, nil
}

// NameConflict is one name held by more than one retained identity: the name,
// and every proxy session ID whose row claims it.
type NameConflict struct {
	// Name is the contested name, as stored (the first spelling encountered).
	Name string
	// ProxySessionIDs are the proxy sessions claiming it, sorted for a stable
	// report.
	ProxySessionIDs []string
}

// LegacyNameConflicts reports names claimed by more than one retained identity.
// nil-safe (returns nil).
//
// Such rows are a legacy artefact, not a bug this release can fix. Before
// retention, a name was unique only among LIVE sessions: a pruned row's name
// could legitimately be redrawn by another proxy, and both rows are now
// retained. There is no evidence in the database saying which claim came first
// in any meaningful sense, and the candidate repairs are all worse than the
// ambiguity — renaming a row would break notes addressed to it, deleting one
// would fork the identity it proves, and picking by updated_at would silently
// hand one session's mailbox to another.
//
// So this REPORTS rather than repairs: the daemon logs the conflict once at
// startup, every unaffected identity migrates and recovers normally, and an
// operator who cares can resolve it deliberately. The new contract prevents new
// collisions; an ambiguous history is not safely repairable from names alone.
func (s *Store) LegacyNameConflicts() ([]NameConflict, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT name, proxy_session_id FROM session_names
		  WHERE plumb_session_id != ''
		  ORDER BY name, proxy_session_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: legacy name conflicts: %w", err)
	}
	defer rows.Close()
	byName := map[string][]string{}
	spelling := map[string]string{}
	for rows.Next() {
		var name, proxyID string
		if err := rows.Scan(&name, &proxyID); err != nil {
			return nil, fmt.Errorf("sessionstate: scan name conflict: %w", err)
		}
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, seen := spelling[key]; !seen {
			spelling[key] = name
		}
		byName[key] = append(byName[key], proxyID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessionstate: legacy name conflicts: %w", err)
	}
	var out []NameConflict
	for key, ids := range byName {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		out = append(out, NameConflict{Name: spelling[key], ProxySessionIDs: ids})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
