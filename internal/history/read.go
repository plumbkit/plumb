package history

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// Filter specifies criteria for querying history changes.
type Filter struct {
	Workspace string
	All       bool
	SessionID string
	Agent     string
	Tool      string
	File      string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
}

// Entry is one change record retrieved from history.db.
type Entry struct {
	Seq          int64
	At           time.Time
	CallID       string
	Workspace    string
	Path         string
	From         string
	Kind         Kind
	Op           Op
	Tool         string
	SessionID    string
	SessionName  string
	LogicalAgent string
	ClientName   string
	BeforeSHA    []byte
	AfterSHA     []byte
	BeforeSize   int64
	AfterSize    int64
	BeforeExists bool
	AfterExists  bool
	Added        int
	Removed      int
	Redactions   int
	Content      Content
	RevertsSeq   int64
	Reason       string
	GapBefore    bool
	GapDropped   bool
}

// Reader provides read-only access to a history database.
type Reader struct {
	db  *sql.DB
	dec *zstd.Decoder
	v   int
}

// OpenReadOnly opens the default global history database for reading.
// Returns (nil, nil) if the file does not exist.
func OpenReadOnly() (*Reader, error) {
	p := DBPath()
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return nil, nil
	}
	return OpenReadOnlyAt(p)
}

// OpenReadOnlyAt opens the history database at path for reading.
func OpenReadOnlyAt(path string) (*Reader, error) {
	db, err := sqlitex.OpenReadOnly(path, sqlitex.ReadOnlyOptions{BusyTimeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("history: open readonly %s: %w", path, err)
	}
	v, err := sqlitex.Version(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("history: read version: %w", err)
	}
	if v > SchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("%w: upgrade plumb to read it (file v%d, this plumb v%d)", ErrNewerSchema, v, SchemaVersion)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("history: zstd reader: %w", err)
	}
	return &Reader{db: db, dec: dec, v: v}, nil
}

// Close closes the underlying database and decoder.
func (r *Reader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	r.dec.Close()
	return r.db.Close()
}

// Version is the history.db schema version this reader opened (0 for a file
// with no schema yet).
func (r *Reader) Version() int {
	if r == nil {
		return 0
	}
	return r.v
}

// Count returns the total number of change rows.
func (r *Reader) Count() (int64, error) {
	if r == nil || r.v == 0 {
		return 0, nil
	}
	var n int64
	err := r.db.QueryRow(`SELECT COUNT(*) FROM changes`).Scan(&n)
	return n, err
}

// Meta returns all key/value pairs from the meta table.
func (r *Reader) Meta() (map[string]string, error) {
	if r == nil || r.v == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func buildListQuery(f Filter) (string, []any) {
	query := `SELECT c.seq, c.ts_ms, c.call_id, w.root, p.path, COALESCE(fp.path,''), c.kind, c.op, c.tool,
		session_id, c.session_name, c.logical_agent, c.client_name, c.before_sha, c.after_sha,
		before_size, c.after_size, c.added, c.removed, c.redactions, c.content,
		COALESCE(c.reverts_seq,0), c.reason, c.path_id
	FROM changes c JOIN workspaces w ON w.id=c.workspace_id JOIN paths p ON p.id=c.path_id
	LEFT JOIN paths fp ON fp.id=c.from_path_id`

	var conds []string
	var args []any

	canonRoot := ""
	if f.Workspace != "" {
		canonRoot = paths.Canonical(f.Workspace)
	}

	if !f.All && canonRoot != "" {
		conds = append(conds, "w.root = ?")
		args = append(args, canonRoot)
	}
	if f.SessionID != "" {
		conds = append(conds, "c.session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.Agent != "" {
		conds = append(conds, "c.logical_agent = ?")
		args = append(args, f.Agent)
	}
	if f.Tool != "" {
		conds = append(conds, "c.tool = ?")
		args = append(args, f.Tool)
	}
	if f.File != "" {
		canonFile := paths.Canonical(f.File)
		if !f.All && canonRoot != "" {
			conds = append(conds, "p.path = ?")
			args = append(args, relTo(canonRoot, canonFile))
		} else {
			// Across workspaces each row's path is relative to ITS root, so an
			// absolute file matches root + "/" + path (or a path stored absolute
			// because it lay outside every workspace).
			conds = append(conds, "(p.path = ? OR w.root || '/' || p.path = ?)")
			args = append(args, canonFile, canonFile)
		}
	}
	if !f.Since.IsZero() {
		conds = append(conds, "c.ts_ms >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "c.ts_ms < ?")
		args = append(args, f.Until.UnixMilli())
	}

	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ") //nolint:gosec // conds are fixed SQL fragments parameterized with ? placeholders
	}
	query += " ORDER BY c.ts_ms DESC, c.seq DESC LIMIT ?"
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit)
	if f.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, f.Offset)
	}
	return query, args
}

type scanResult struct {
	entry  Entry
	pathID int64
}

func scanListEntry(rows *sql.Rows) (Entry, int64, error) {
	var (
		e          Entry
		tsMs       int64
		pathID     int64
		kindStr    string
		opStr      string
		contentStr string
		bSize      sql.NullInt64
		aSize      sql.NullInt64
	)
	err := rows.Scan(
		&e.Seq, &tsMs, &e.CallID, &e.Workspace, &e.Path, &e.From, &kindStr, &opStr, &e.Tool,
		&e.SessionID, &e.SessionName, &e.LogicalAgent, &e.ClientName, &e.BeforeSHA, &e.AfterSHA,
		&bSize, &aSize, &e.Added, &e.Removed, &e.Redactions, &contentStr,
		&e.RevertsSeq, &e.Reason, &pathID,
	)
	if err != nil {
		return Entry{}, 0, err
	}
	populateEntry(&e, tsMs, kindStr, opStr, contentStr, bSize, aSize)
	return e, pathID, nil
}

func populateEntry(e *Entry, tsMs int64, kindStr, opStr, contentStr string, bSize, aSize sql.NullInt64) {
	e.At = time.UnixMilli(tsMs)
	e.Kind = Kind(kindStr)
	e.Op = Op(opStr)
	e.Content = Content(contentStr)
	if bSize.Valid {
		e.BeforeExists = true
		e.BeforeSize = bSize.Int64
	} else if len(e.BeforeSHA) > 0 {
		e.BeforeExists = true
	}
	if aSize.Valid {
		e.AfterExists = true
		e.AfterSize = aSize.Int64
	} else if len(e.AfterSHA) > 0 {
		e.AfterExists = true
	}
}

// prevStateSQL finds a path's state just before seq ?2: the newest earlier row
// that left path ?1 in a known state. That is a row ON the path (its after
// side), or a rename AWAY from it, which left the path absent (NULL). A
// copy from the path does not change it, so only renames count.
const prevStateSQL = `SELECT CASE WHEN path_id = ?1 THEN after_sha ELSE NULL END, ts_ms
	FROM changes
	WHERE seq < ?2 AND (path_id = ?1 OR (from_path_id = ?1 AND op = 'rename'))
	ORDER BY seq DESC LIMIT 1`

// chainFor is the path whose chain e's before side continues: its own, or for
// a rename its source's.
func chainFor(fromStmt *sql.Stmt, e Entry, pathID int64) (int64, error) {
	if e.Op != OpRename {
		return pathID, nil
	}
	var from sql.NullInt64
	if err := fromStmt.QueryRow(e.Seq).Scan(&from); err != nil {
		return 0, fmt.Errorf("history: check gap: %w", err)
	}
	if !from.Valid {
		return pathID, nil
	}
	return from.Int64, nil
}

// detectGaps marks each entry whose before side does not continue its path's
// chain. A rename row's before is its SOURCE's content, so it is checked
// against the source path's chain, not the destination's (whose previous
// state the rename replaced).
func (r *Reader) detectGaps(scanned []scanResult) ([]Entry, error) {
	gapStmt, err := r.db.Prepare(prevStateSQL)
	if err != nil {
		return nil, fmt.Errorf("history: prepare gap query: %w", err)
	}
	defer gapStmt.Close()
	fromStmt, err := r.db.Prepare(`SELECT from_path_id FROM changes WHERE seq=?`)
	if err != nil {
		return nil, fmt.Errorf("history: prepare gap query: %w", err)
	}
	defer fromStmt.Close()

	var lastDropMs int64
	if meta, err := r.Meta(); err == nil && meta != nil {
		if s, ok := meta["last_drop_at_ms"]; ok {
			lastDropMs, _ = strconv.ParseInt(s, 10, 64)
		}
	}

	out := make([]Entry, len(scanned))
	for i, s := range scanned {
		e := s.entry
		chain, err := chainFor(fromStmt, e, s.pathID)
		if err != nil {
			return nil, err
		}
		var prevAfter []byte
		var prevTsMs int64
		err = gapStmt.QueryRow(chain, e.Seq).Scan(&prevAfter, &prevTsMs)
		if err == nil {
			if !bytes.Equal(prevAfter, e.BeforeSHA) {
				e.GapBefore = true
				if lastDropMs > 0 && lastDropMs >= prevTsMs && lastDropMs <= e.At.UnixMilli() {
					e.GapDropped = true
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("history: check gap: %w", err)
		}
		out[i] = e
	}
	return out, nil
}

// List returns entries matching the filter, ordered newest first (ts_ms DESC, seq DESC).
func (r *Reader) List(f Filter) ([]Entry, error) {
	if r == nil || r.v == 0 {
		return nil, nil
	}
	query, args := buildListQuery(f)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("history: list changes: %w", err)
	}
	defer rows.Close()

	var scanned []scanResult
	for rows.Next() {
		e, pathID, err := scanListEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("history: scan entry: %w", err)
		}
		scanned = append(scanned, scanResult{entry: e, pathID: pathID})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return r.detectGaps(scanned)
}

// Get returns the entry and decompressed unified diff for a given sequence number.
func (r *Reader) Get(seq int64) (Entry, string, error) {
	if r == nil || r.v == 0 {
		return Entry{}, "", sql.ErrNoRows
	}
	row := r.db.QueryRow(`SELECT c.seq, c.ts_ms, c.call_id, w.root, p.path, COALESCE(fp.path,''), c.kind, c.op, c.tool,
		session_id, c.session_name, c.logical_agent, c.client_name, c.before_sha, c.after_sha,
		before_size, c.after_size, c.added, c.removed, c.redactions, c.content,
		COALESCE(c.reverts_seq,0), c.reason, c.diff
	FROM changes c JOIN workspaces w ON w.id=c.workspace_id JOIN paths p ON p.id=c.path_id
	LEFT JOIN paths fp ON fp.id=c.from_path_id
	WHERE c.seq = ?`, seq)

	var (
		e          Entry
		tsMs       int64
		kindStr    string
		opStr      string
		contentStr string
		bSize      sql.NullInt64
		aSize      sql.NullInt64
		rawDiff    []byte
	)
	err := row.Scan(
		&e.Seq, &tsMs, &e.CallID, &e.Workspace, &e.Path, &e.From, &kindStr, &opStr, &e.Tool,
		&e.SessionID, &e.SessionName, &e.LogicalAgent, &e.ClientName, &e.BeforeSHA, &e.AfterSHA,
		&bSize, &aSize, &e.Added, &e.Removed, &e.Redactions, &contentStr,
		&e.RevertsSeq, &e.Reason, &rawDiff,
	)
	if err != nil {
		return Entry{}, "", err
	}
	populateEntry(&e, tsMs, kindStr, opStr, contentStr, bSize, aSize)

	diffText := ""
	if e.Content == ContentDiff && len(rawDiff) > 0 {
		plain, err := r.dec.DecodeAll(rawDiff, nil)
		if err != nil {
			return Entry{}, "", fmt.Errorf("history: decompress diff: %w", err)
		}
		diffText = string(plain)
	}
	return e, diffText, nil
}

// ByCall returns all entries and decompressed diffs for a given call ID in seq order.
func (r *Reader) ByCall(callID string) ([]Entry, []string, error) {
	return r.byCall(callID, true)
}

// CallEntries is ByCall without the diffs: the call's entries in seq order,
// nothing read or decompressed, for a caller that pages or budgets its output
// and fetches only the diffs it shows (Get). A call can touch thousands of
// files, each diff up to the 4 MiB cap.
func (r *Reader) CallEntries(callID string) ([]Entry, error) {
	entries, _, err := r.byCall(callID, false)
	return entries, err
}

func (r *Reader) byCall(callID string, withDiffs bool) ([]Entry, []string, error) {
	if r == nil || r.v == 0 {
		return nil, nil, nil
	}
	diffCol := "NULL"
	if withDiffs {
		diffCol = "c.diff"
	}
	rows, err := r.db.Query(`SELECT c.seq, c.ts_ms, c.call_id, w.root, p.path, COALESCE(fp.path,''), c.kind, c.op, c.tool,
		session_id, c.session_name, c.logical_agent, c.client_name, c.before_sha, c.after_sha,
		before_size, c.after_size, c.added, c.removed, c.redactions, c.content,
		COALESCE(c.reverts_seq,0), c.reason, `+diffCol+`
	FROM changes c JOIN workspaces w ON w.id=c.workspace_id JOIN paths p ON p.id=c.path_id
	LEFT JOIN paths fp ON fp.id=c.from_path_id
	WHERE c.call_id = ? ORDER BY c.seq ASC`, callID)
	if err != nil {
		return nil, nil, fmt.Errorf("history: by call: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	var diffs []string
	for rows.Next() {
		var (
			e          Entry
			tsMs       int64
			kindStr    string
			opStr      string
			contentStr string
			bSize      sql.NullInt64
			aSize      sql.NullInt64
			rawDiff    []byte
		)
		err := rows.Scan(
			&e.Seq, &tsMs, &e.CallID, &e.Workspace, &e.Path, &e.From, &kindStr, &opStr, &e.Tool,
			&e.SessionID, &e.SessionName, &e.LogicalAgent, &e.ClientName, &e.BeforeSHA, &e.AfterSHA,
			&bSize, &aSize, &e.Added, &e.Removed, &e.Redactions, &contentStr,
			&e.RevertsSeq, &e.Reason, &rawDiff,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("history: scan by call: %w", err)
		}
		populateEntry(&e, tsMs, kindStr, opStr, contentStr, bSize, aSize)

		diffText := ""
		if e.Content == ContentDiff && len(rawDiff) > 0 {
			plain, err := r.dec.DecodeAll(rawDiff, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("history: decompress diff: %w", err)
			}
			diffText = string(plain)
		}
		entries = append(entries, e)
		diffs = append(diffs, diffText)
	}
	return entries, diffs, rows.Err()
}
