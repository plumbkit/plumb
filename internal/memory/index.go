package memory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plumbkit/plumb/internal/paths"

	_ "modernc.org/sqlite" // register the SQLite driver
)

// memorySchemaVersion is the on-disk schema generation. memory.db is a
// rebuildable index over the markdown files, so an incompatible bump is handled
// by recreating from the files rather than migrating.
const memorySchemaVersion = 1

// Confidence labels how a memory came to exist. User-authored memories rank
// above generated ones on a search tie.
type Confidence string

const (
	ConfidenceUser      Confidence = "user"
	ConfidenceGenerated Confidence = "generated"
	ConfidenceImported  Confidence = "imported"
	ConfidenceInferred  Confidence = "inferred"
)

// Record is the indexable view of one memory: file-derived content plus
// provenance/lifecycle metadata. The markdown file remains the source of truth;
// a Record is what the index stores about it.
type Record struct {
	Name          string
	Description   string
	Body          string
	Paths         []string
	SourcePaths   []string
	SourceSymbols []string
	SourceSession string
	SourceCalls   []string
	Confidence    Confidence
	ContentSHA    string
	MTimeNS       int64
	SizeBytes     int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	StaleAfter    time.Time
}

const memorySchema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS memory_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS memory_files (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    content_sha TEXT    NOT NULL DEFAULT '',
    mtime_ns    INTEGER NOT NULL DEFAULT 0,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    indexed_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS memory_records (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id        INTEGER NOT NULL REFERENCES memory_files(id) ON DELETE CASCADE,
    name           TEXT    NOT NULL UNIQUE,
    description    TEXT    NOT NULL DEFAULT '',
    paths_json     TEXT    NOT NULL DEFAULT '',
    source_paths   TEXT    NOT NULL DEFAULT '',
    source_symbols TEXT    NOT NULL DEFAULT '',
    source_session TEXT    NOT NULL DEFAULT '',
    source_calls   TEXT    NOT NULL DEFAULT '',
    confidence     TEXT    NOT NULL DEFAULT 'user',
    content_sha    TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL DEFAULT 0,
    last_used_at   INTEGER NOT NULL DEFAULT 0,
    supersedes     TEXT    NOT NULL DEFAULT '',
    superseded_by  TEXT    NOT NULL DEFAULT '',
    stale_after    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_mr_conf ON memory_records(confidence);
CREATE INDEX IF NOT EXISTS idx_mr_used ON memory_records(last_used_at);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
    name,
    name_tokens,
    description,
    body,
    path_globs,
    source_paths,
    source_symbols,
    provenance,
    tokenize='unicode61 remove_diacritics 2'
);
`

// Index is a per-workspace SQLite/FTS5 index over the markdown memory store.
//
// Concurrency: a single mutex serialises every write (the modernc SQLite driver
// is opened with one connection). Read queries also take the lock; memory sets
// are tiny so contention is negligible.
type Index struct {
	mu        sync.Mutex
	db        *sql.DB
	workspace string
	// reindexing guards ReindexAsync: at most one background reindex per index.
	reindexing atomic.Bool
}

// IndexDBPath returns the canonical path to the memory index for a workspace.
func IndexDBPath(workspace string) string {
	return filepath.Join(workspace, ".plumb", "memory.db")
}

// OpenIndex opens or creates the memory index for workspace.
func OpenIndex(workspace string) (*Index, error) {
	path := IndexDBPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("memory: create db dir: %w", err)
	}
	ensureMemoryGitignore(filepath.Dir(path))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("memory: open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := initMemoryDB(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db, workspace: workspace}, nil
}

func initMemoryDB(db *sql.DB) error {
	for _, pragma := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
		fmt.Sprintf(`PRAGMA user_version = %d`, memorySchemaVersion),
	} {
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("memory: pragma %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(memorySchema); err != nil {
		return fmt.Errorf("memory: apply schema: %w", err)
	}
	return nil
}

// Workspace returns the workspace this index belongs to.
func (ix *Index) Workspace() string {
	if ix == nil {
		return ""
	}
	return ix.workspace
}

// Close releases the underlying database. It takes ix.mu so the close never
// overlaps a concurrent Upsert/Remove/Reindex on the same handle (they all hold
// ix.mu during their db work) — e.g. the daemon's shutdown CloseAll firing while
// a just-Acquired index is mid background reindex. ix.db is deliberately NOT
// nilled: a Reindex that acquires the mutex after Close then sees a *closed*
// (non-nil) handle and fails cleanly with "sql: database is closed" (logged and
// swallowed by the best-effort callers), rather than nil-dereferencing.
func (ix *Index) Close() error {
	if ix == nil {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.db == nil {
		return nil
	}
	return ix.db.Close()
}

// Fresh reports whether the index matches the markdown files on disk, comparing
// each file's full-content sha256 against the stored hash. List already reads
// every file's bytes (for the frontmatter parse) and hashes them, so this is an
// exact content check at near-zero extra cost — and unlike an mtime+size anchor
// it cannot be fooled by a same-size edit that leaves the mtime untouched. A
// drift (new, deleted, or modified file) returns false; the caller then
// Reindexes or falls back to grep.
func (ix *Index) Fresh(workspace string) (bool, error) {
	mems, err := List(workspace)
	if err != nil {
		return false, err
	}
	onDisk := make(map[string]Memory, len(mems))
	for _, m := range mems {
		onDisk[m.Name] = m
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	rows, err := ix.db.Query(`SELECT name, content_sha FROM memory_files`)
	if err != nil {
		return false, fmt.Errorf("memory: read file anchors: %w", err)
	}
	defer rows.Close()
	indexed := 0
	for rows.Next() {
		var name, sha string
		if err := rows.Scan(&name, &sha); err != nil {
			continue
		}
		indexed++
		m, ok := onDisk[name]
		if !ok {
			return false, nil // indexed a memory that no longer exists on disk
		}
		if m.ContentSHA == "" || m.ContentSHA != sha {
			return false, nil // unreadable or content drifted
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return indexed == len(onDisk), nil // any un-indexed on-disk memory ⇒ stale
}

// Reindex brings the index into line with the markdown files: it upserts new or
// changed memories and removes index rows for memories deleted on disk. Returns
// the number of files (re)indexed.
//
// List already read every file's bytes and computed Memory.ContentSHA, so the
// unchanged-file path compares that SHA against the stored anchor (one cheap
// pre-loaded map lookup) and only calls recordFromFile — the full re-read — for
// memories whose content drifted or that are absent from the index. An unchanged
// memory therefore costs the single List read, not a second read plus a re-hash.
func (ix *Index) Reindex(workspace string) (int, error) {
	mems, err := List(workspace)
	if err != nil {
		return 0, err
	}
	anchors, err := ix.fileAnchors()
	if err != nil {
		return 0, err
	}
	live := make(map[string]bool, len(mems))
	changed := 0
	for _, m := range mems {
		live[m.Name] = true
		if stored, ok := anchors[m.Name]; ok && m.ContentSHA != "" && m.ContentSHA == stored {
			continue // unchanged on disk — skip the second read
		}
		rec, err := readRecord(workspace, m.Name)
		if err != nil {
			continue
		}
		if err := ix.Upsert(rec); err != nil {
			return changed, err
		}
		changed++
	}
	if err := ix.removeMissing(live); err != nil {
		return changed, err
	}
	return changed, nil
}

// fileAnchors loads the stored name → content_sha map in one query, so Reindex
// can decide whether each on-disk memory has drifted without a per-memory DB
// round-trip.
func (ix *Index) fileAnchors() (map[string]string, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	rows, err := ix.db.Query(`SELECT name, content_sha FROM memory_files`)
	if err != nil {
		return nil, fmt.Errorf("memory: read file anchors: %w", err)
	}
	defer rows.Close()
	anchors := make(map[string]string)
	for rows.Next() {
		var name, sha string
		if err := rows.Scan(&name, &sha); err != nil {
			continue
		}
		anchors[name] = sha
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: read file anchors: %w", err)
	}
	return anchors, nil
}

// ReindexAsync runs Reindex in the background, at most one in flight per index
// (a CAS guard drops the call when a reindex is already running). Used to
// self-heal a stale index after an auto-mode search fell back to grep, so the
// next query can use FTS. Errors are logged at Debug, not returned.
func (ix *Index) ReindexAsync(workspace string) {
	if ix == nil || !ix.reindexing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer ix.reindexing.Store(false)
		if _, err := ix.Reindex(workspace); err != nil {
			slog.Debug("memory: async reindex failed", "workspace", workspace, "err", err)
		}
	}()
}

func (ix *Index) removeMissing(live map[string]bool) error {
	ix.mu.Lock()
	rows, err := ix.db.Query(`SELECT name FROM memory_files`)
	if err != nil {
		ix.mu.Unlock()
		return fmt.Errorf("memory: list indexed files: %w", err)
	}
	var gone []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && !live[name] {
			gone = append(gone, name)
		}
	}
	rows.Close()
	ix.mu.Unlock()
	for _, name := range gone {
		if err := ix.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

// readRecord is the indirection Reindex calls to do the full file read; it is a
// var so a test can count how often Reindex actually re-reads a file (the
// unchanged-file path must not).
var readRecord = recordFromFile

// recordFromFile reads a memory file and builds its Record. Provenance fields
// are populated from frontmatter where present (generated memories carry them);
// a plain user memory yields confidence=user with empty provenance.
func recordFromFile(workspace, name string) (Record, error) {
	path, err := Path(workspace, name)
	if err != nil {
		return Record{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("memory: read %q: %w", name, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return Record{}, err
	}
	fm, body := splitFrontmatter(data)
	parsed := parseFrontmatterFull(data)
	rec := Record{
		Name:        name,
		Description: parsed.description,
		Body:        string(body),
		Paths:       parsed.paths,
		Confidence:  ConfidenceUser,
		ContentSHA:  sha256Hex(data),
		MTimeNS:     st.ModTime().UnixNano(),
		SizeBytes:   st.Size(),
	}
	applyProvenanceFrontmatter(&rec, fm)
	return rec, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ensureMemoryGitignore makes sure dir/.gitignore excludes memory.db and its
// SQLite sidecars. Unlike topology.db, memory.db is durable project knowledge —
// but it is still a derived index that must not be committed (the markdown files
// are the source of truth). Best-effort; failure is non-fatal.
func ensureMemoryGitignore(dir string) {
	_ = paths.EnsureGitignoreEntries(dir,
		"# plumb memory index (durable; rebuilt from memories/*.md; do not commit)",
		[]string{"memory.db", "memory.db-wal", "memory.db-shm"})
}
