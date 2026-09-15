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

// RepairExternalID refills a durable record's BLANK external linkage from a
// source the caller has already authenticated, and reports whether it did.
// nil-safe.
//
// The narrowness is the whole safety argument. The record is the only proof of
// what a reconnecting session should come back as, and every identity fork this
// package exists to prevent arrived through a write that fired when it should
// not have. So this is a single conditional UPDATE rather than a save: it lands
// only when the row still names the session ID the caller proved, and only when
// the stored linkage is still blank. A known linkage is never overwritten here
// — replacement is SaveIdentity's job, on the live session's own authority — and
// a row that moved on between the caller's read and this write simply does not
// match, so the caller is told the repair did not land rather than seeing a
// silent success.
//
// NameRevision is deliberately untouched: the name did not change, and a
// revision bump would misorder concurrent name snapshots.
func (s *Store) RepairExternalID(proxySessionID, sessionID, externalID string) (bool, error) {
	if s == nil || proxySessionID == "" || sessionID == "" || externalID == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE session_names
		    SET external_id=?, updated_at=?
		  WHERE proxy_session_id=? AND plumb_session_id=? AND external_id=''`,
		externalID, time.Now().UnixMilli(), proxySessionID, sessionID,
	)
	if err != nil {
		return false, fmt.Errorf("sessionstate: repair external id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sessionstate: repair external id: %w", err)
	}
	return n > 0, nil
}

// claim is one retained row's claim on a name, with enough of the record to order
// it against that conversation's other generations.
type claim struct {
	Name       string
	ProxyID    string
	SessionID  string
	ExternalID string
	UpdatedAt  int64
}

// independentClaims drops every row a NEWER row of the same conversation and name
// has superseded, keeping only the newest of each such group.
//
// This is the whole of this package's "retirement", and it deliberately retires
// the claim rather than the row. A superseded row is still the durable proof of
// which plumb session a reconnecting `plumb serve` is, and "a newer row exists" is
// not proof that the older serve died: a serve whose socket dropped is
// unregistered while its process stays alive and its proxy secret stays valid,
// and Store.Prune's own comment states the rule this obeys — "Age is no evidence
// that a serve process died; only the serve itself knows that". Deleting or
// blanking the row would turn that serve's next reconnect into first contact: a
// new session ID and a new name, with every note bound to the old ID stranded.
//
// So a superseded generation is retired from the ANSWERS — the name it holds, and
// the ambiguity it reports — and not from the table.
//
// What is proven is deliberately the conservative half: a NON-BLANK external_id
// on both rows (a blank linkage is UNKNOWN, not a conversation) and the same name
// compared case-insensitively, because that is how name uniqueness itself is
// compared. A name held by two DIFFERENT conversations keeps both claims: the
// database holds no evidence of which should win, and choosing would hand one
// session's reserved name — and the mail bound to it — to another.
//
// "Newest" is (updated_at, proxy session ID). SaveIdentity refreshes updated_at on
// every save, so the survivor is the row whose serve was seen most recently, and
// the proxy-ID tie-break keeps the outcome deterministic when two rows share a
// millisecond rather than depending on the order rows happen to come back in.
func independentClaims(rows []claim) []claim {
	newest := make(map[string]claim, len(rows))
	key := func(r claim) string { return strings.ToLower(r.Name) + "\x00" + r.ExternalID }
	for _, r := range rows {
		cur, ok := newest[key(r)]
		if !ok || r.UpdatedAt > cur.UpdatedAt || (r.UpdatedAt == cur.UpdatedAt && r.ProxyID > cur.ProxyID) {
			newest[key(r)] = r
		}
	}
	out := make([]claim, 0, len(rows))
	for _, r := range rows {
		// The blank-linkage rule is enforced HERE and only here: a row it cannot
		// group is passed through untouched. Both callers drop empty names as they
		// scan, so this does not repeat that test.
		if r.ExternalID == "" || newest[key(r)].ProxyID == r.ProxyID {
			out = append(out, r)
		}
	}
	return out
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
// A conversation's own superseded generations are collapsed: when several rows
// share a name AND a non-blank external ID, only the newest holds the claim (see
// independentClaims). The name is then held for the conversation that owns it,
// the answer does not depend on the order rows come back in, and every row stays
// intact as the proof of which session a reconnecting proxy is.
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
		`SELECT name, plumb_session_id, external_id, proxy_session_id, updated_at
		   FROM session_names WHERE plumb_session_id != ''`,
	)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: reservations: %w", err)
	}
	defer rows.Close()
	var claims []claim
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.Name, &c.SessionID, &c.ExternalID, &c.ProxyID, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sessionstate: scan reservation: %w", err)
		}
		if c.Name == "" {
			continue
		}
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessionstate: reservations: %w", err)
	}
	out := make([]Reservation, 0, len(claims))
	for _, c := range independentClaims(claims) {
		out = append(out, Reservation{Name: c.Name, SessionID: c.SessionID, ExternalID: c.ExternalID})
	}
	return out, nil
}

// NameConflict is one name held by more than one retained identity: the name,
// and every proxy session ID whose surviving claim holds it (see
// independentClaims — a conversation's own superseded generations are collapsed
// out before this is built).
type NameConflict struct {
	// Name is the contested name, as stored (the first spelling encountered).
	Name string
	// ProxySessionIDs are the proxy sessions claiming it, sorted for a stable
	// report.
	ProxySessionIDs []string
}

// LegacyNameConflicts reports names whose claim cannot be shown to belong to one
// conversation. nil-safe (returns nil).
//
// What it reports is a name held by rows that share no conversation: two
// different external IDs, or a linkage that is blank and therefore unknown. The
// database holds no evidence of which claim should win, and the candidate
// repairs are all worse than the ambiguity — renaming a row leaves notes with no
// bound identity following the name, deleting one forks the identity it proves,
// and picking by updated_at would silently hand one session's name and mailbox to
// another.
//
// A conversation's own superseded generations are NOT reported: they share a
// non-blank linkage and a name, so independentClaims keeps only the newest of
// them. Retention never prevented those generations being written — a
// `plumb serve` restart mints a second row for a name its predecessor still held —
// so calling them a pre-retention artefact was wrong twice over, and the warning
// that named them at every daemon start buried the genuinely ambiguous names this
// function exists to surface.
//
// So this REPORTS rather than repairs: the daemon logs the residue once at
// startup, every unaffected identity migrates and recovers normally, and an
// operator who cares can resolve it deliberately. An ambiguous history is not
// safely repairable from names alone.
func (s *Store) LegacyNameConflicts() ([]NameConflict, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT name, proxy_session_id, plumb_session_id, external_id, updated_at
		   FROM session_names
		  WHERE plumb_session_id != ''
		  ORDER BY name, proxy_session_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: legacy name conflicts: %w", err)
	}
	defer rows.Close()
	var claims []claim
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.Name, &c.ProxyID, &c.SessionID, &c.ExternalID, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sessionstate: scan name conflict: %w", err)
		}
		if c.Name == "" {
			continue
		}
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessionstate: legacy name conflicts: %w", err)
	}
	byName := map[string][]string{}
	spelling := map[string]string{}
	for _, c := range independentClaims(claims) {
		key := strings.ToLower(c.Name)
		if _, seen := spelling[key]; !seen {
			spelling[key] = c.Name
		}
		byName[key] = append(byName[key], c.ProxyID)
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
