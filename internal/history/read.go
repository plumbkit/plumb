package history

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
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
		filePath := canonFile
		if !f.All && canonRoot != "" {
			filePath = relTo(canonRoot, canonFile)
		}
		conds = append(conds, "p.path = ?")
		args = append(args, filePath)
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

func (r *Reader) detectGaps(scanned []scanResult) ([]Entry, error) {
	gapStmt, err := r.db.Prepare(`SELECT after_sha FROM changes WHERE path_id=? AND seq<? ORDER BY seq DESC LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("history: prepare gap query: %w", err)
	}
	defer gapStmt.Close()

	out := make([]Entry, len(scanned))
	for i, s := range scanned {
		e := s.entry
		var prevAfter []byte
		err := gapStmt.QueryRow(s.pathID, e.Seq).Scan(&prevAfter)
		if err == nil {
			if !bytes.Equal(prevAfter, e.BeforeSHA) {
				e.GapBefore = true
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
	if r == nil || r.v == 0 {
		return nil, nil, nil
	}
	rows, err := r.db.Query(`SELECT c.seq, c.ts_ms, c.call_id, w.root, p.path, COALESCE(fp.path,''), c.kind, c.op, c.tool,
		session_id, c.session_name, c.logical_agent, c.client_name, c.before_sha, c.after_sha,
		before_size, c.after_size, c.added, c.removed, c.redactions, c.content,
		COALESCE(c.reverts_seq,0), c.reason, c.diff
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
