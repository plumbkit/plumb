package history

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/redact"
	"github.com/plumbkit/plumb/internal/textdiff"
)

const maxLinkEntries = 4096

func (s *Store) run() {
	defer close(s.done)
	tick := time.NewTicker(s.opts.BatchWait)
	defer tick.Stop()
	var batch []Item
	flushAll := func() {
		s.writeBatch(s.takeQueued(batch))
		batch = nil
	}
	for {
		s.waitIfPaused()
		select {
		case it, ok := <-s.ch:
			if !ok {
				flushAll()
				return
			}
			batch = append(batch, it)
			if len(batch) >= s.opts.BatchSize {
				s.writeBatch(batch)
				batch = nil
			}
		case <-tick.C:
			flushAll()
		case reply := <-s.syncReq:
			flushAll()
			close(reply)
		}
	}
}

type rendered struct {
	content                    Content
	diff                       []byte
	added, removed, redactions int
}

// render classifies and encodes it (spec §5.2 steps 3–5). Pre-classified items
// pass through; past Close's deadline everything degrades to a marker.
func (s *Store) render(it Item, maxDiff int64) rendered {
	r := rendered{content: it.Content, added: it.Added, removed: it.Removed}
	if s.degrade.Load() && r.content == "" {
		r.content = ContentOverflow
	}
	if r.content != "" {
		return r
	}
	b, a := it.Before.Content, it.After.Content
	if isBinary(b) || isBinary(a) {
		r.content = ContentBinary
		return r
	}
	script := textdiff.ComputeExact(string(b), string(a))
	r.added, r.removed = textdiff.Counts(script)
	if r.added == 0 && r.removed == 0 {
		r.content = ContentNone
		return r
	}
	text := textdiff.Render(script)
	if int64(len(text)) > maxDiff {
		r.content = ContentTooLarge
		return r
	}
	red, n := redact.Redact(text)
	r.redactions = n
	r.diff = s.enc.EncodeAll([]byte(red), nil)
	r.content = ContentDiff
	return r
}

// landedRow is one landed row's (call, path) → seq entry, remembered only once the
// batch commits: a rolled-back seq is reused by AUTOINCREMENT, so remembering it
// early would link a later revert to another call's row.
type landedRow struct {
	key string
	seq int64
}

// batchTally is what one batch must persist to meta.
type batchTally struct {
	drops, errs, overflow int64
	dropAt                time.Time
	lastErr               error
}

// writeBatch commits items in one transaction. Each row runs under a SAVEPOINT:
// a failing row is retried as a marker; a marker that fails too is dropped and
// counted — never retried, so a poison item cannot wedge the writer.
//
// No count is ever cleared without reaching meta: the carried-over counts are
// taken only once the transaction has begun, and a batch that cannot persist
// them (or loses its rows outright) gives them back, with its own losses, for
// the next batch to write.
func (s *Store) writeBatch(items []Item) {
	if len(items) == 0 && !s.hasPendingCounts() {
		return
	}
	defer s.release(items)
	tx, err := s.db.Begin()
	if err != nil {
		slog.Warn("history: begin batch", "err", err, "rows", len(items))
		s.giveBack(int64(len(items)), time.Now(), 0)
		return
	}
	var t batchTally
	t.drops, t.dropAt, t.errs = s.takeCounts()
	links := s.insertAll(tx, items, &t)
	// A failed meta write does not sink the rows: they still commit, and the
	// counts go back (exactly once, below) for the next batch to persist.
	metaErr := s.persistMeta(tx, t)
	if err := tx.Commit(); err != nil {
		slog.Warn("history: commit batch", "err", err, "rows", len(items))
		_ = tx.Rollback()
		// Lost: every row that had landed (t.drops already holds the ones that
		// never did) — so each item is counted once.
		s.giveBack(t.drops+int64(len(links)), time.Now(), t.errs)
		return
	}
	if metaErr != nil {
		slog.Warn("history: persist meta", "err", metaErr)
		s.giveBack(t.drops, t.dropAt, t.errs)
	}
	for _, l := range links {
		s.remember(l.key, l.seq)
	}
}

// insertAll inserts each item (retrying a failure as a marker) and tallies what
// meta must record. An overflow row is counted only once it has landed.
func (s *Store) insertAll(tx *sql.Tx, items []Item, t *batchTally) []landedRow {
	maxDiff := s.opts.MaxDiffBytes()
	var links []landedRow
	for _, it := range items {
		r := s.render(it, maxDiff)
		l, err := s.insertSavepoint(tx, it, r)
		if err == nil {
			if r.content == ContentOverflow {
				t.overflow++
			}
			links = append(links, l)
			continue
		}
		t.errs++
		t.lastErr = err
		// The retry is metadata only, and drops the revert link too: a link
		// that failed its foreign key the first time fails it again.
		m := marker(it)
		m.RevertsOwnCall, m.RevertsSHA = false, nil
		if l, err := s.insertSavepoint(tx, m, rendered{content: ContentOverflow}); err == nil {
			t.overflow++
			links = append(links, l)
		} else {
			t.drops++
			t.dropAt = time.Now()
		}
	}
	return links
}

func (s *Store) insertSavepoint(tx *sql.Tx, it Item, r rendered) (landedRow, error) {
	if _, err := tx.Exec(`SAVEPOINT row`); err != nil {
		return landedRow{}, err
	}
	l, err := s.insert(tx, it, r)
	if err != nil {
		_, _ = tx.Exec(`ROLLBACK TO row`)
		_, _ = tx.Exec(`RELEASE row`)
		return landedRow{}, err
	}
	_, err = tx.Exec(`RELEASE row`)
	return l, err
}

func (s *Store) insert(tx *sql.Tx, it Item, r rendered) (landedRow, error) {
	root := paths.Canonical(it.Workspace)
	wsID, err := internWorkspace(tx, root)
	if err != nil {
		return landedRow{}, err
	}
	path := paths.Canonical(it.Path)
	pathID, err := internPath(tx, wsID, relTo(root, path))
	if err != nil {
		return landedRow{}, err
	}
	var fromID sql.NullInt64
	if it.From != "" {
		id, err := internPath(tx, wsID, relTo(root, paths.Canonical(it.From)))
		if err != nil {
			return landedRow{}, err
		}
		fromID = sql.NullInt64{Int64: id, Valid: true}
	}
	reverts := s.revertTarget(tx, it, path, pathID)
	res, err := tx.Exec(`INSERT INTO changes (ts_ms, call_id, workspace_id, path_id, from_path_id, kind, op, tool,
		session_id, session_name, logical_agent, client_name, before_sha, after_sha, before_size, after_size,
		added, removed, content, redactions, diff, reverts_seq, reason)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.At.UnixMilli(), it.CallID, wsID, pathID, fromID, kindOrFile(it.Kind), string(it.Op), it.Tool,
		it.SessionID, it.SessionName, it.LogicalAgent, it.ClientName,
		shaOrNil(it.Before), shaOrNil(it.After), sizeOrNil(it.Before), sizeOrNil(it.After),
		r.added, r.removed, string(r.content), r.redactions, r.diff, reverts, it.Reason)
	if err != nil {
		return landedRow{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return landedRow{}, err
	}
	return landedRow{key: it.CallID + "\x00" + path, seq: seq}, nil
}

// revertTarget resolves reverts_seq on the writer (spec §5.1): own-call reverts
// via the in-memory map, falling back to the DB (crash recovery across a
// restart); undo_edit by the sha plumb wrote.
func (s *Store) revertTarget(tx *sql.Tx, it Item, path string, pathID int64) sql.NullInt64 {
	var seq int64
	var err error
	switch {
	case it.RevertsOwnCall:
		// A remembered seq may since have been pruned; trust it only if the
		// row is still there, else fall back to the query below.
		if v, ok := s.link[it.CallID+"\x00"+path]; ok {
			var one int
			if tx.QueryRow(`SELECT 1 FROM changes WHERE seq=?`, v).Scan(&one) == nil {
				return sql.NullInt64{Int64: v, Valid: true}
			}
		}
		err = tx.QueryRow(`SELECT seq FROM changes WHERE call_id=? AND path_id=? ORDER BY seq DESC LIMIT 1`, it.CallID, pathID).Scan(&seq)
	case len(it.RevertsSHA) > 0:
		err = tx.QueryRow(`SELECT seq FROM changes WHERE path_id=? AND after_sha=? ORDER BY seq DESC LIMIT 1`, pathID, it.RevertsSHA).Scan(&seq)
	default:
		return sql.NullInt64{}
	}
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Debug("history: revert lookup", "err", err)
		}
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: seq, Valid: true}
}

func (s *Store) remember(key string, seq int64) {
	if _, ok := s.link[key]; !ok {
		s.linkKeys = append(s.linkKeys, key)
		if len(s.linkKeys) > maxLinkEntries {
			delete(s.link, s.linkKeys[0])
			s.linkKeys = s.linkKeys[1:]
		}
	}
	s.link[key] = seq
}

func internWorkspace(tx *sql.Tx, root string) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO workspaces(root) VALUES (?) ON CONFLICT(root) DO NOTHING`, root); err != nil {
		return 0, err
	}
	var id int64
	return id, tx.QueryRow(`SELECT id FROM workspaces WHERE root=?`, root).Scan(&id)
}

func internPath(tx *sql.Tx, wsID int64, p string) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO paths(workspace_id, path) VALUES (?,?) ON CONFLICT(workspace_id, path) DO NOTHING`, wsID, p); err != nil {
		return 0, err
	}
	var id int64
	return id, tx.QueryRow(`SELECT id FROM paths WHERE workspace_id=? AND path=?`, wsID, p).Scan(&id)
}

// relTo stores a path relative to its workspace when inside it, else absolute.
func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.ToSlash(rel)
}

func kindOrFile(k Kind) string {
	if k == "" {
		return string(KindFile)
	}
	return string(k)
}

func shaOrNil(s Side) any {
	if !s.Exists {
		return nil
	}
	return s.SHA
}

func sizeOrNil(s Side) any {
	if !s.Exists {
		return nil
	}
	return s.Size
}

// persistMeta writes the batch's tally and reports the first failure, so the
// caller can give the counts back rather than lose them.
func (s *Store) persistMeta(tx *sql.Tx, t batchTally) error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	bump := func(key string, n int64) {
		if n == 0 {
			return
		}
		_, err := tx.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + ?`, key, strconv.FormatInt(n, 10), n)
		keep(err)
	}
	set := func(key, v string) {
		_, err := tx.Exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, v)
		keep(err)
	}
	bump("dropped_rows", t.drops)
	if t.drops > 0 {
		set("last_drop_at_ms", strconv.FormatInt(t.dropAt.UnixMilli(), 10))
	}
	bump("overflow_rows", t.overflow)
	bump("write_errors", t.errs)
	if t.lastErr != nil {
		slog.Warn("history: row insert failed", "err", t.lastErr, "count", t.errs)
		set("last_error", t.lastErr.Error())
		set("last_error_at_ms", strconv.FormatInt(time.Now().UnixMilli(), 10))
	}
	return first
}
