// Package contexthints is the bounded observation ledger behind plumb's advisory
// context hints (PLAN-462 Slice B), and the allowance that caps how much hint
// text any one logical agent can be sent.
//
// It is the ONLY persistence the hint path has, and it is deliberately small and
// dumb: metadata about each hook invocation (who, where, which event, which
// capped selectors, how many bytes were emitted, how it ended), never the hint
// text, never source, never read state. Its job is to make invocation measurable
// when the hook runs over the daemon's control socket, which bypasses stats.db,
// and to stop a session from earning a fresh allowance by being evicted,
// compacted or restarted.
//
// Bounds, per workspace: at most MaxRowsPerWorkspace rows and
// MaxBytesPerWorkspace bytes of row payload, each row kept at most Retention.
// Every row that leaves early, by either cap or by age, is counted, so a missing
// row is reported as evicted rather than read as a no-op.
//
// The allowance lives in its own table, outside the row caps: evicting an
// agent's observations must not refill what it has already spent. An allowance
// row ages out only after Retention without a spend.
//
// A separate database file from stats.db and session_state.db: its lifecycle
// (caps, retention) and privacy rule (metadata only) are its own.
package contexthints

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// Ledger bounds. They are part of the PLAN-462 gate, so they are constants, not
// configuration.
const (
	MaxRowsPerWorkspace  = 1000
	MaxBytesPerWorkspace = 1 << 20
	Retention            = 7 * 24 * time.Hour
	MaxSeeds             = 8
	MaxSeedBytes         = 512
)

// Outcome is how one hook invocation ended.
type Outcome string

const (
	OutcomeEmitted   Outcome = "emitted"
	OutcomeNoop      Outcome = "noop"
	OutcomeThrottled Outcome = "throttled"
	OutcomeError     Outcome = "error"
)

func (o Outcome) valid() bool {
	switch o {
	case OutcomeEmitted, OutcomeNoop, OutcomeThrottled, OutcomeError:
		return true
	}
	return false
}

// Observation is one hook invocation. Every caller-controlled text field is
// either a code (checked against codeRe), an identifier (capped), or a seed
// selector (capped in count and bytes); there is no field for prose.
type Observation struct {
	At           time.Time
	Workspace    string // canonical root, or "" when it could not be resolved
	SessionID    string
	AgentID      string // "" for a conversation's main thread
	Host         string // e.g. claude-code, codex
	HostVersion  string
	Event        string // the host's hook event name
	Source       string // SessionStart's source, when the host sent one
	Seeds        []string
	Emitted      []string // selectors the hint named (capped like Seeds): what consumption matches
	EmittedBytes int
	Duration     time.Duration
	Outcome      Outcome
	// Detail is a short machine code qualifying Outcome ("off", "cold-index",
	// "no-seeds"), never free text.
	Detail string
}

var (
	codeRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{0,64}$`)
	detailRe = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)
)

const (
	maxIDBytes        = 256
	maxWorkspaceBytes = 1024
	rowOverhead       = 64 // integers and bookkeeping, counted against the byte cap
)

const schema = `
CREATE TABLE IF NOT EXISTS obs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace     TEXT    NOT NULL,
    at_ms         INTEGER NOT NULL,
    session_id    TEXT    NOT NULL,
    agent_id      TEXT    NOT NULL,
    host          TEXT    NOT NULL,
    host_version  TEXT    NOT NULL,
    event         TEXT    NOT NULL,
    source        TEXT    NOT NULL,
    seeds         TEXT    NOT NULL,
    emitted       TEXT    NOT NULL DEFAULT '',
    emitted_bytes INTEGER NOT NULL,
    duration_ms   INTEGER NOT NULL,
    outcome       TEXT    NOT NULL,
    detail        TEXT    NOT NULL,
    size          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_obs_ws_at ON obs(workspace, at_ms, id);
CREATE INDEX IF NOT EXISTS idx_obs_at ON obs(at_ms);

CREATE TABLE IF NOT EXISTS evictions (
    workspace  TEXT    PRIMARY KEY,
    evicted    INTEGER NOT NULL,
    updated_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS allowance (
    session_id  TEXT    NOT NULL,
    agent_id    TEXT    NOT NULL,
    total_bytes INTEGER NOT NULL,
    turn_key    TEXT    NOT NULL,
    turn_bytes  INTEGER NOT NULL,
    updated_ms  INTEGER NOT NULL,
    PRIMARY KEY (session_id, agent_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_allowance_updated ON allowance(updated_ms);
`

// SchemaVersion is stamped in PRAGMA user_version. History:
//
//	1 — obs, evictions, allowance
const SchemaVersion = 1

// Store is the ledger. All methods are safe for concurrent use and nil-safe.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// DBPath is the ledger's path in the persistent data directory, a sibling of
// stats.db and session_state.db.
func DBPath() string {
	return filepath.Join(config.DataDir(), "context_hints.db")
}

// Open opens (or creates) the ledger at its conventional path.
func Open() (*Store, error) { return openAt(DBPath()) }

// OpenAt opens (or creates) the ledger at an explicit path: for tests, and for
// an isolated daemon whose data directory is elsewhere.
func OpenAt(path string) (*Store, error) { return openAt(path) }

// OpenReadOnly opens an existing ledger for reading only, as a report outside
// the daemon does: it can never create the file, write a row or take the
// writer's lock. A ledger that does not exist yet is (nil, nil), which every
// read method treats as empty.
func OpenReadOnly(path string) (*Store, error) {
	if path == "" {
		path = DBPath()
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	db, err := sqlitex.OpenReadOnly(path, sqlitex.ReadOnlyOptions{})
	if err != nil {
		return nil, fmt.Errorf("contexthints: open read-only %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func openAt(path string) (*Store, error) {
	// SyncNormal under WAL: a commit survives a process exit, which is the
	// restart the allowance must outlive; losing the last commit to a power cut
	// costs one observation.
	db, err := sqlitex.Open(path, sqlitex.Options{Sync: sqlitex.SyncNormal, MaxOpenConns: 1})
	if err != nil {
		return nil, fmt.Errorf("contexthints: open %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("contexthints: schema: %w", err)
	}
	v, err := sqlitex.Version(db)
	if err == nil && v < SchemaVersion {
		err = sqlitex.StampVersion(db, SchemaVersion)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("contexthints: version: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database. nil-safe.
func (s *Store) Close() {
	if s != nil && s.db != nil {
		_ = s.db.Close()
	}
}

// capSeeds keeps at most MaxSeeds selectors whose bytes total at most
// MaxSeedBytes, in order, dropping (never truncating) any that would not fit or
// that span lines. A truncated selector would name something else.
func capSeeds(seeds []string) []string {
	var out []string
	total := 0
	for _, s := range seeds {
		if len(out) == MaxSeeds {
			break
		}
		if s == "" || strings.ContainsAny(s, "\r\n") || total+len(s) > MaxSeedBytes {
			continue
		}
		out = append(out, s)
		total += len(s)
	}
	return out
}

func seedBytes(seeds []string) int {
	n := 0
	for _, s := range seeds {
		n += len(s)
	}
	return n
}

// rowSize is what one row counts against MaxBytesPerWorkspace.
func rowSize(o Observation) int {
	return rowOverhead + len(o.Workspace) + len(o.SessionID) + len(o.AgentID) + len(o.Host) +
		len(o.HostVersion) + len(o.Event) + len(o.Source) + seedBytes(o.Seeds) + seedBytes(o.Emitted) + len(o.Detail)
}

func (o Observation) validate() error {
	switch {
	case !o.Outcome.valid():
		return fmt.Errorf("contexthints: unknown outcome %q", o.Outcome)
	case !detailRe.MatchString(o.Detail):
		return errors.New("contexthints: detail must be a short code")
	case !codeRe.MatchString(o.Host), !codeRe.MatchString(o.HostVersion),
		!codeRe.MatchString(o.Event), !codeRe.MatchString(o.Source):
		return errors.New("contexthints: host, version, event and source must be codes")
	case len(o.SessionID) > maxIDBytes, len(o.AgentID) > maxIDBytes, len(o.Workspace) > maxWorkspaceBytes:
		return errors.New("contexthints: identifier too long")
	case o.EmittedBytes < 0, o.Duration < 0:
		return errors.New("contexthints: negative measure")
	}
	return nil
}

// Record appends one observation, then evicts the workspace's oldest rows until
// it is back within both caps, counting what it evicted. nil-safe.
func (s *Store) Record(o Observation) error {
	if s == nil {
		return nil
	}
	o.Seeds = capSeeds(o.Seeds)
	o.Emitted = capSeeds(o.Emitted)
	if err := o.validate(); err != nil {
		return err
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("contexthints: record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO obs (workspace, at_ms, session_id, agent_id, host, host_version, event, source,
	                                       seeds, emitted, emitted_bytes, duration_ms, outcome, detail, size)
	                      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Workspace, o.At.UnixMilli(), o.SessionID, o.AgentID, o.Host, o.HostVersion, o.Event, o.Source,
		strings.Join(o.Seeds, "\n"), strings.Join(o.Emitted, "\n"), o.EmittedBytes, o.Duration.Milliseconds(),
		string(o.Outcome), o.Detail, rowSize(o)); err != nil {
		return fmt.Errorf("contexthints: record: %w", err)
	}
	if err := enforceCaps(tx, o.Workspace, o.At); err != nil {
		return err
	}
	return tx.Commit()
}

// enforceCaps deletes a workspace's oldest rows until it holds at most
// MaxRowsPerWorkspace rows and MaxBytesPerWorkspace bytes.
func enforceCaps(tx *sql.Tx, workspace string, now time.Time) error {
	var rows, bytes int
	if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM obs WHERE workspace=?`, workspace).
		Scan(&rows, &bytes); err != nil {
		return fmt.Errorf("contexthints: caps: %w", err)
	}
	if rows <= MaxRowsPerWorkspace && bytes <= MaxBytesPerWorkspace {
		return nil
	}
	q, err := tx.Query(`SELECT id, size FROM obs WHERE workspace=? ORDER BY at_ms, id`, workspace)
	if err != nil {
		return fmt.Errorf("contexthints: caps: %w", err)
	}
	var doomed []int64
	for q.Next() && (rows > MaxRowsPerWorkspace || bytes > MaxBytesPerWorkspace) {
		var id int64
		var size int
		if err := q.Scan(&id, &size); err != nil {
			q.Close()
			return fmt.Errorf("contexthints: caps: %w", err)
		}
		doomed = append(doomed, id)
		rows--
		bytes -= size
	}
	if err := q.Close(); err != nil {
		return fmt.Errorf("contexthints: caps: %w", err)
	}
	for _, id := range doomed {
		if _, err := tx.Exec(`DELETE FROM obs WHERE id=?`, id); err != nil {
			return fmt.Errorf("contexthints: evict: %w", err)
		}
	}
	return countEvicted(tx, workspace, len(doomed), now)
}

func countEvicted(tx *sql.Tx, workspace string, n int, now time.Time) error {
	if n == 0 {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO evictions (workspace, evicted, updated_ms) VALUES (?, ?, ?)
	                   ON CONFLICT(workspace) DO UPDATE SET evicted = evicted + excluded.evicted,
	                                                        updated_ms = excluded.updated_ms`,
		workspace, n, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("contexthints: count evicted: %w", err)
	}
	return nil
}

// Prune ages out observations and idle allowances older than Retention,
// counting aged-out observations as evicted. nil-safe.
func (s *Store) Prune(now time.Time) error {
	if s == nil {
		return nil
	}
	cutoff := now.Add(-Retention).UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("contexthints: prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q, err := tx.Query(`SELECT workspace, COUNT(*) FROM obs WHERE at_ms < ? GROUP BY workspace`, cutoff)
	if err != nil {
		return fmt.Errorf("contexthints: prune: %w", err)
	}
	aged := map[string]int{}
	for q.Next() {
		var ws string
		var n int
		if err := q.Scan(&ws, &n); err != nil {
			q.Close()
			return fmt.Errorf("contexthints: prune: %w", err)
		}
		aged[ws] = n
	}
	if err := q.Close(); err != nil {
		return fmt.Errorf("contexthints: prune: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM obs WHERE at_ms < ?`, cutoff); err != nil {
		return fmt.Errorf("contexthints: prune: %w", err)
	}
	for ws, n := range aged {
		if err := countEvicted(tx, ws, n, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM allowance WHERE updated_ms < ?`, cutoff); err != nil {
		return fmt.Errorf("contexthints: prune allowances: %w", err)
	}
	return tx.Commit()
}

// Limits are the two hint allowances: per turn window and per logical agent.
type Limits struct {
	PerTurn  int
	PerAgent int
}

// Spend charges n bytes to (sessionID, agentID) in turn window turnKey when
// both allowances can take it, and reports whether it did. A refused spend
// charges nothing. A new turnKey opens a fresh per-turn window; nothing opens a
// fresh per-agent allowance short of Retention without a spend. nil-safe: a nil
// store grants nothing, so a hint path with no ledger fails closed.
func (s *Store) Spend(sessionID, agentID, turnKey string, n int, lim Limits, now time.Time) (bool, error) {
	if s == nil || n < 0 {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("contexthints: spend: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var total, turnBytes int
	var curTurn string
	switch err := tx.QueryRow(`SELECT total_bytes, turn_key, turn_bytes FROM allowance WHERE session_id=? AND agent_id=?`,
		sessionID, agentID).Scan(&total, &curTurn, &turnBytes); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("contexthints: spend: %w", err)
	}
	if curTurn != turnKey {
		turnBytes = 0
	}
	if turnBytes+n > lim.PerTurn || total+n > lim.PerAgent {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO allowance (session_id, agent_id, total_bytes, turn_key, turn_bytes, updated_ms)
	                      VALUES (?, ?, ?, ?, ?, ?)
	                      ON CONFLICT(session_id, agent_id) DO UPDATE SET total_bytes=excluded.total_bytes,
	                          turn_key=excluded.turn_key, turn_bytes=excluded.turn_bytes, updated_ms=excluded.updated_ms`,
		sessionID, agentID, total+n, turnKey, turnBytes+n, now.UnixMilli()); err != nil {
		return false, fmt.Errorf("contexthints: spend: %w", err)
	}
	return true, tx.Commit()
}

// Summary is a workspace's ledger state.
type Summary struct {
	Rows      int
	Bytes     int
	ByOutcome map[Outcome]int
	Evicted   int
}

// Summary reports a workspace's row and byte counts, rows per outcome, and how
// many rows it has lost to the caps or to age. nil-safe.
func (s *Store) Summary(workspace string) (Summary, error) {
	sum := Summary{ByOutcome: map[Outcome]int{}}
	if s == nil {
		return sum, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q, err := s.db.Query(`SELECT outcome, COUNT(*), COALESCE(SUM(size), 0) FROM obs WHERE workspace=? GROUP BY outcome`, workspace)
	if err != nil {
		return sum, fmt.Errorf("contexthints: summary: %w", err)
	}
	defer q.Close()
	for q.Next() {
		var o string
		var n, b int
		if err := q.Scan(&o, &n, &b); err != nil {
			return sum, fmt.Errorf("contexthints: summary: %w", err)
		}
		sum.ByOutcome[Outcome(o)] = n
		sum.Rows += n
		sum.Bytes += b
	}
	if err := q.Err(); err != nil {
		return sum, fmt.Errorf("contexthints: summary: %w", err)
	}
	switch err := s.db.QueryRow(`SELECT evicted FROM evictions WHERE workspace=?`, workspace).Scan(&sum.Evicted); {
	case err == nil, errors.Is(err, sql.ErrNoRows):
	default:
		return sum, fmt.Errorf("contexthints: summary: %w", err)
	}
	return sum, nil
}

// recent returns a workspace's newest n observations, newest first.
func (s *Store) recent(workspace string, n int) ([]Observation, error) {
	return s.query(`WHERE workspace=? ORDER BY at_ms DESC, id DESC LIMIT ?`, workspace, n)
}

// Since returns every observation at or after since, oldest first, across all
// workspaces: the input to an uptake report. nil-safe.
func (s *Store) Since(since time.Time) ([]Observation, error) {
	if s == nil {
		return nil, nil
	}
	return s.query(`WHERE at_ms >= ? ORDER BY at_ms, id`, since.UnixMilli())
}

func (s *Store) query(where string, args ...any) ([]Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	//nolint:gosec // G202: where is a fixed fragment from this file; values are bound
	q, err := s.db.Query(`SELECT workspace, at_ms, session_id, agent_id, host, host_version, event, source, seeds,
	                             emitted, emitted_bytes, duration_ms, outcome, detail
	                      FROM obs `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("contexthints: read: %w", err)
	}
	defer q.Close()
	var out []Observation
	for q.Next() {
		var o Observation
		var at, dur int64
		var seeds, emitted, outcome string
		if err := q.Scan(&o.Workspace, &at, &o.SessionID, &o.AgentID, &o.Host, &o.HostVersion, &o.Event, &o.Source,
			&seeds, &emitted, &o.EmittedBytes, &dur, &outcome, &o.Detail); err != nil {
			return nil, fmt.Errorf("contexthints: read: %w", err)
		}
		o.At, o.Duration, o.Outcome = time.UnixMilli(at), time.Duration(dur)*time.Millisecond, Outcome(outcome)
		o.Seeds, o.Emitted = splitLines(seeds), splitLines(emitted)
		out = append(out, o)
	}
	return out, q.Err()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
