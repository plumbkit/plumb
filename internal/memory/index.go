package memory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

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

// Close releases the index database.
func (ix *Index) Close() error {
	if ix == nil || ix.db == nil {
		return nil
	}
	return ix.db.Close()
}

// Fresh reports whether the index matches the markdown files on disk, comparing
// each file's mtime and size against the stored anchor. It deliberately does not
// read bodies, so it is cheap enough to gate every search. A drift (new, deleted,
// or modified file) returns false; the caller then Reindexes or falls back to grep.
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
	rows, err := ix.db.Query(`SELECT name, mtime_ns, size_bytes FROM memory_files`)
	if err != nil {
		return false, fmt.Errorf("memory: read file anchors: %w", err)
	}
	defer rows.Close()
	indexed := 0
	for rows.Next() {
		var name string
		var mtime, size int64
		if err := rows.Scan(&name, &mtime, &size); err != nil {
			continue
		}
		indexed++
		m, ok := onDisk[name]
		if !ok {
			return false, nil // indexed a memory that no longer exists on disk
		}
		st, err := os.Stat(m.Path)
		if err != nil || st.ModTime().UnixNano() != mtime || st.Size() != size {
			return false, nil
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
func (ix *Index) Reindex(workspace string) (int, error) {
	mems, err := List(workspace)
	if err != nil {
		return 0, err
	}
	live := make(map[string]bool, len(mems))
	changed := 0
	for _, m := range mems {
		live[m.Name] = true
		rec, err := recordFromFile(workspace, m.Name)
		if err != nil {
			continue
		}
		if ix.isCurrent(m.Name, rec.ContentSHA) {
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

func (ix *Index) isCurrent(name, sha string) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var stored string
	err := ix.db.QueryRow(`SELECT content_sha FROM memory_files WHERE name = ?`, name).Scan(&stored)
	return err == nil && stored == sha
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
	desc, paths := parseFrontmatterFull(data)
	rec := Record{
		Name:        name,
		Description: desc,
		Body:        string(body),
		Paths:       paths,
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

// splitIdentifier splits CamelCase / snake_case / kebab-case / dotted / slashed
// identifiers into space-separated lowercase tokens for FTS indexing, so a query
// for "user session" matches a memory named "UserSession".
//
// NOTE: copied from internal/topology to avoid a memory→topology import (they are
// sibling packages). TestSplitIdentifier_ParityWithTopology guards the copy.
// Phase 2 may extract a shared internal/tokenise leaf.
func splitIdentifier(s string) string {
	if s == "" {
		return ""
	}
	s = strings.NewReplacer("_", " ", "-", " ", ".", " ", "/", " ").Replace(s)
	var buf strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && runes[i-1] != ' ' {
			lowerToUpper := unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1])
			upperSeqToLower := unicode.IsUpper(r) && unicode.IsUpper(runes[i-1]) &&
				i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if lowerToUpper || upperSeqToLower {
				buf.WriteRune(' ')
			}
		}
		buf.WriteRune(unicode.ToLower(r))
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}

// ensureMemoryGitignore makes sure dir/.gitignore excludes memory.db and its
// SQLite sidecars. Unlike topology.db, memory.db is durable project knowledge —
// but it is still a derived index that must not be committed (the markdown files
// are the source of truth). Best-effort; failure is non-fatal.
func ensureMemoryGitignore(dir string) {
	const header = "# plumb memory index (durable; rebuilt from memories/*.md; do not commit)"
	entries := []string{"memory.db", "memory.db-wal", "memory.db-shm"}
	path := filepath.Join(dir, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	have := make(map[string]bool)
	for line := range strings.SplitSeq(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, e := range entries {
		if !have[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteByte('\n')
	}
	if !have[header] {
		b.WriteString(header + "\n")
	}
	for _, e := range missing {
		b.WriteString(e + "\n")
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644) //nolint:gosec // G306: .gitignore is a normal repo file
}
