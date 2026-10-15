package sessionstate

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A namespace is a private slice of the one daemon's per-connection state, owned
// by whoever holds its token (PLAN-520, the owner's decision E2: a local
// end-to-end run is isolated by a namespace inside the one daemon, not by a
// daemon of its own). Every sessionstate table is keyed by proxy session ID, so a
// namespace needs no column on them: it is the set of proxy sessions that joined
// it (namespace_member), plus the workspace roots its members wrote shared rows
// in (namespace_root), so a purge can reach every row they left behind.

// NamespaceTokenPrefix marks a namespace token, so one pasted in the wrong place
// is recognisable and is never mistaken for a resume secret ("rsk1-").
const NamespaceTokenPrefix = "pns1-"

// Namespace states. A revoked namespace refuses its token at once; its rows
// survive only until the purge that follows the drain.
const (
	NamespaceActive  = "active"
	NamespaceRevoked = "revoked"
)

// Namespace is one persisted namespace. The token is never stored, only its
// hash; ID is the public handle shown in listings and logs.
type Namespace struct {
	ID         string
	TokenHash  string
	TTL        time.Duration
	CreatedAt  time.Time
	ExpiresAt  time.Time
	OwnerPID   int
	OwnerStart int64 // the owner process's start time, as the platform reports it; 0 when unknown
	// OwnerMismatchAt is when the owner hint first stopped matching a live
	// process, or zero while it matches. The namespace expires a grace after it.
	OwnerMismatchAt time.Time
	State           string
}

// ErrNamespaceUnknown is returned for a token or ID no namespace has.
var ErrNamespaceUnknown = errors.New("sessionstate: no such namespace")

// NewNamespaceToken returns a fresh token (128 random bits) and its public ID
// (32 more), each unguessable on its own: the token authenticates, the ID only
// names.
func NewNamespaceToken() (token, id string, err error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("sessionstate: namespace token: %w", err)
	}
	return NamespaceTokenPrefix + hex.EncodeToString(b[:16]), "ns-" + hex.EncodeToString(b[16:]), nil
}

// HashNamespaceToken is the stored form of a token.
func HashNamespaceToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// migrateV12 adds the namespace tables. Additive: an older binary ignores them,
// and every existing row stays in the default namespace (it belongs to no
// namespace_member row), which is the whole migration.
func migrateV12(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS namespace (
    id                TEXT    NOT NULL PRIMARY KEY,
    token_hash        TEXT    NOT NULL,
    ttl_ms            INTEGER NOT NULL,
    created_at        INTEGER NOT NULL,
    expires_at        INTEGER NOT NULL,
    owner_pid         INTEGER NOT NULL DEFAULT 0,
    owner_start       INTEGER NOT NULL DEFAULT 0,
    owner_mismatch_at INTEGER NOT NULL DEFAULT 0,
    state             TEXT    NOT NULL
) WITHOUT ROWID`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_ns_token ON namespace(token_hash)`,
		`CREATE TABLE IF NOT EXISTS namespace_member (
    namespace_id     TEXT NOT NULL,
    proxy_session_id TEXT NOT NULL PRIMARY KEY
) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_nsm_ns ON namespace_member(namespace_id)`,
		`CREATE TABLE IF NOT EXISTS namespace_root (
    namespace_id TEXT NOT NULL,
    root         TEXT NOT NULL,
    PRIMARY KEY (namespace_id, root)
) WITHOUT ROWID`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("sessionstate: migrate v12 (namespaces): %w", err)
		}
	}
	return nil
}

const namespaceCols = `id, token_hash, ttl_ms, created_at, expires_at, owner_pid, owner_start, owner_mismatch_at, state`

func scanNamespace(row interface{ Scan(...any) error }) (Namespace, error) {
	var ns Namespace
	var ttl, created, expires, mismatch int64
	if err := row.Scan(&ns.ID, &ns.TokenHash, &ttl, &created, &expires, &ns.OwnerPID, &ns.OwnerStart, &mismatch, &ns.State); err != nil {
		return Namespace{}, err
	}
	ns.TTL = time.Duration(ttl) * time.Millisecond
	ns.CreatedAt = time.UnixMilli(created)
	ns.ExpiresAt = time.UnixMilli(expires)
	if mismatch != 0 {
		ns.OwnerMismatchAt = time.UnixMilli(mismatch)
	}
	return ns, nil
}

// CreateNamespace persists a new active namespace.
func (s *Store) CreateNamespace(ns Namespace) error {
	if s == nil {
		return errors.New("sessionstate: no store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO namespace (`+namespaceCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		ns.ID, ns.TokenHash, ns.TTL.Milliseconds(), ns.CreatedAt.UnixMilli(), ns.ExpiresAt.UnixMilli(), ns.OwnerPID, ns.OwnerStart, NamespaceActive)
	if err != nil {
		return fmt.Errorf("sessionstate: create namespace: %w", err)
	}
	return nil
}

// NamespaceByToken returns the namespace a token belongs to, in any state; the
// caller decides what a revoked or expired one means. ErrNamespaceUnknown when
// no namespace has it.
func (s *Store) NamespaceByToken(token string) (Namespace, error) {
	return s.namespaceWhere(`token_hash = ?`, HashNamespaceToken(token))
}

// NamespaceByID returns the namespace with that public ID.
func (s *Store) NamespaceByID(id string) (Namespace, error) {
	return s.namespaceWhere(`id = ?`, id)
}

func (s *Store) namespaceWhere(cond string, arg string) (Namespace, error) {
	if s == nil {
		return Namespace{}, ErrNamespaceUnknown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, err := scanNamespace(s.db.QueryRow(`SELECT `+namespaceCols+` FROM namespace WHERE `+cond, arg)) //nolint:gosec // G202: cond is one of two constant predicates
	if errors.Is(err, sql.ErrNoRows) {
		return Namespace{}, ErrNamespaceUnknown
	}
	if err != nil {
		return Namespace{}, fmt.Errorf("sessionstate: load namespace: %w", err)
	}
	return ns, nil
}

// ListNamespaces returns every persisted namespace, oldest first.
func (s *Store) ListNamespaces() ([]Namespace, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT ` + namespaceCols + ` FROM namespace ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: list namespaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Namespace
	for rows.Next() {
		ns, err := scanNamespace(rows)
		if err != nil {
			return nil, fmt.Errorf("sessionstate: scan namespace: %w", err)
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// SetNamespaceExpiry moves an active namespace's expiry (a renewal).
func (s *Store) SetNamespaceExpiry(id string, expiresAt time.Time) error {
	return s.execNamespace(`UPDATE namespace SET expires_at = ? WHERE id = ? AND state = ?`, expiresAt.UnixMilli(), id, NamespaceActive)
}

// SetOwnerMismatch records when the owner hint first stopped matching, or
// clears it with the zero time.
func (s *Store) SetOwnerMismatch(id string, at time.Time) error {
	var ms int64
	if !at.IsZero() {
		ms = at.UnixMilli()
	}
	return s.execNamespace(`UPDATE namespace SET owner_mismatch_at = ? WHERE id = ?`, ms, id)
}

// RevokeNamespace marks a namespace revoked: from here its token is refused.
func (s *Store) RevokeNamespace(id string) error {
	return s.execNamespace(`UPDATE namespace SET state = ? WHERE id = ?`, NamespaceRevoked, id)
}

// AddNamespaceMember records that a proxy session joined a namespace. A proxy
// session belongs to one namespace for life: a second, different namespace for
// the same ID is refused, never moved.
func (s *Store) AddNamespaceMember(id, proxySessionID string) error {
	if s == nil || proxySessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var have string
	err := s.db.QueryRow(`SELECT namespace_id FROM namespace_member WHERE proxy_session_id = ?`, proxySessionID).Scan(&have)
	switch {
	case err == nil && have == id:
		return nil
	case err == nil:
		return fmt.Errorf("sessionstate: proxy session already belongs to namespace %s", have)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("sessionstate: namespace member: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO namespace_member (namespace_id, proxy_session_id) VALUES (?, ?)`, id, proxySessionID); err != nil {
		return fmt.Errorf("sessionstate: add namespace member: %w", err)
	}
	return nil
}

// NamespaceOf returns the namespace a proxy session belongs to, or "" (the
// default namespace).
func (s *Store) NamespaceOf(proxySessionID string) (string, error) {
	if s == nil || proxySessionID == "" {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var id string
	err := s.db.QueryRow(`SELECT namespace_id FROM namespace_member WHERE proxy_session_id = ?`, proxySessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sessionstate: namespace of: %w", err)
	}
	return id, nil
}

// NamespaceMembers returns the proxy sessions that joined a namespace.
func (s *Store) NamespaceMembers(id string) ([]string, error) {
	return s.stringsWhere(`SELECT proxy_session_id FROM namespace_member WHERE namespace_id = ? ORDER BY proxy_session_id`, id)
}

// NamespaceSessionIDs returns the plumb session IDs the namespace's members
// held — each connection's own, and each logical agent's roster row — so a purge
// can remove their session files, which live outside this database.
func (s *Store) NamespaceSessionIDs(id string) ([]string, error) {
	return s.stringsWhere(`SELECT plumb_session_id FROM session_names
	WHERE plumb_session_id != '' AND proxy_session_id IN (SELECT proxy_session_id FROM namespace_member WHERE namespace_id = ?1)
UNION SELECT roster_session_id FROM logical_agent
	WHERE roster_session_id != '' AND proxy_session_id IN (SELECT proxy_session_id FROM namespace_member WHERE namespace_id = ?1)
ORDER BY 1`, id)
}

// AddNamespaceRoot records a workspace root a member wrote shared rows under,
// so the purge can open it.
func (s *Store) AddNamespaceRoot(id, root string) error {
	if s == nil || id == "" || root == "" {
		return nil
	}
	return s.execNamespace(`INSERT OR IGNORE INTO namespace_root (namespace_id, root) VALUES (?, ?)`, id, root)
}

// NamespaceRoots returns the roots recorded for a namespace.
func (s *Store) NamespaceRoots(id string) ([]string, error) {
	return s.stringsWhere(`SELECT root FROM namespace_root WHERE namespace_id = ? ORDER BY root`, id)
}

// PurgeNamespace deletes every row a namespace's members left in this store —
// reads, pins, logical agents, declared linkages, resume credentials and
// identity records — then the namespace itself, in one transaction. It returns
// the members' proxy session IDs so the caller can remove what lives outside
// this database. Rows of the default namespace are untouched: they belong to no
// member.
func (s *Store) PurgeNamespace(id string) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("sessionstate: purge namespace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var members []string
	rows, err := tx.Query(`SELECT proxy_session_id FROM namespace_member WHERE namespace_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: purge namespace members: %w", err)
	}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("sessionstate: purge namespace members: %w", err)
		}
		members = append(members, m)
	}
	_ = rows.Close()
	const memberOf = ` WHERE proxy_session_id IN (SELECT proxy_session_id FROM namespace_member WHERE namespace_id = ?)`
	for _, table := range namespacedTables {
		if _, err := tx.Exec(`DELETE FROM `+table+memberOf, id); err != nil { //nolint:gosec // G202: table is from the constant list below
			return nil, fmt.Errorf("sessionstate: purge namespace %s: %w", table, err)
		}
	}
	for _, q := range []string{
		`DELETE FROM namespace_member WHERE namespace_id = ?`,
		`DELETE FROM namespace_root WHERE namespace_id = ?`,
		`DELETE FROM namespace WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return nil, fmt.Errorf("sessionstate: purge namespace: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sessionstate: purge namespace commit: %w", err)
	}
	return members, nil
}

// namespacedTables is every table keyed by proxy session ID: the rows a
// namespace's members can own. A new such table must be added here, or a purge
// leaves its rows behind (TestPurgeNamespace_CoversEveryProxySessionTable pins it).
var namespacedTables = []string{
	"read_tracking",
	"pinned_workspace",
	"logical_agent",
	"declared_linkage",
	"resume_credential",
	"session_names",
}

func (s *Store) execNamespace(q string, args ...any) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(q, args...); err != nil {
		return fmt.Errorf("sessionstate: namespace: %w", err)
	}
	return nil
}

func (s *Store) stringsWhere(q, arg string) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(q, arg)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: namespace query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("sessionstate: namespace scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
