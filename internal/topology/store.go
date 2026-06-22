package topology

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

// Store is the public API for topology. It owns a database connection and
// an Indexer. Obtain one via Open; release resources with Close.
//
// Concurrency: Store is safe for concurrent use after Open returns.
type Store struct {
	workspace string
	db        *sql.DB
	idx       *Indexer
	watcher   *fsWatcher // nil when [topology] watch is off or the OS watcher could not start
}

// Open opens or creates the topology index for workspace. It starts the
// background indexer automatically. The caller must call Close when done.
func Open(workspace string, cfg config.TopologyConfig, exts []Extractor) (*Store, error) {
	if workspace == "" {
		return nil, fmt.Errorf("topology: workspace path is empty")
	}
	dbPath := DBPath(workspace)
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	idx := newIndexer(workspace, db, exts, cfg.MaxFileSizeBytes, cfg.ResyncIntervalMinutes)
	idx.resyncBatch = cfg.ResyncBatch
	idx.resyncPause = time.Duration(cfg.ResyncPauseMs) * time.Millisecond
	s := &Store{workspace: workspace, db: db, idx: idx}

	var watcher *fsWatcher
	if cfg.Watch {
		w, werr := newFSWatcher(workspace, s)
		if werr != nil {
			slog.Warn("topology: file watcher unavailable; falling back to periodic resync",
				"workspace", workspace, "err", werr)
		} else {
			// Event-driven freshness replaces time-based polling: suppress the
			// periodic resync. A full resync still runs once at startup (Indexer.Start)
			// and whenever the OS reports dropped/overflowed events.
			idx.resyncMins = 0
			s.watcher = w
			watcher = w
		}
	}

	idx.Start() // starts the consumer and enqueues the initial full resync
	if watcher != nil {
		watcher.Start() // begin watching once the consumer is ready
	}
	return s, nil
}

// Close stops the indexer and closes the database. Safe to call from any
// goroutine; subsequent calls are no-ops.
func (s *Store) Close() error {
	if s.watcher != nil {
		s.watcher.Stop() // stop enqueuing before the indexer drains and the db closes
	}
	s.idx.Stop()
	return s.db.Close()
}

// Enqueue schedules a file for re-indexing. path may be absolute or workspace-relative.
// Non-blocking; drops silently if the queue is full.
// When the file no longer exists on disk, processUpsert detects the ENOENT and
// automatically routes to processDelete — callers do not need a separate delete call.
func (s *Store) Enqueue(path string) {
	rel := s.toRelative(path)
	s.idx.Enqueue(rel, opUpsert)
}

// Resync triggers a full workspace resync in the background.
func (s *Store) Resync() {
	s.idx.Enqueue("", opResync)
}

// SymbolsInFile returns the indexed symbols for a single file, ordered by start
// line. path may be absolute, a file:// URI, or workspace-relative. Returns an
// empty slice (no error) when the file is not in the index.
func (s *Store) SymbolsInFile(ctx context.Context, path string) ([]Node, error) {
	rel := s.toRelative(paths.URIToPath(path))
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.kind, n.name, n.qualified, n.signature, n.start_line, n.end_line, n.language, f.path,
		       n.has_bytes, n.start_byte, n.end_byte, n.start_col, n.end_col, n.doc_start_byte, n.doc_end_byte
		FROM topology_nodes n
		JOIN topology_files f ON f.id = n.file_id
		WHERE f.path = ?
		ORDER BY n.start_line`, rel)
	if err != nil {
		return nil, fmt.Errorf("topology: symbols in file: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var kind string
		var hasBytes int
		if scanErr := rows.Scan(&kind, &n.Name, &n.Qualified, &n.Signature,
			&n.StartLine, &n.EndLine, &n.Language, &n.Path,
			&hasBytes, &n.StartByte, &n.EndByte, &n.StartCol, &n.EndCol, &n.DocStartByte, &n.DocEndByte); scanErr != nil {
			continue
		}
		n.Kind = NodeKind(kind)
		n.HasBytes = hasBytes != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// Search performs a ranked FTS5 search over the topology index.
func (s *Store) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	return Search(ctx, s.db, query, opts)
}

// TestsInDirs returns the indexed test nodes (KindTest) whose file sits directly
// in one of the given workspace-relative directories. It is the recall booster
// for topology_affected: an extractor only emits intra-file call edges, so a
// test in a sibling file (Go `foo_test.go`, Python `test_foo.py`) that exercises
// a changed symbol is not graph-connected — but it is co-located, which is a
// strong (if heuristic) signal it should be run. Directories are compared by
// exact parent match, so subdirectory tests are not pulled in.
func (s *Store) TestsInDirs(ctx context.Context, dirs []string) ([]Node, error) {
	if len(dirs) == 0 {
		return nil, nil
	}
	want := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		want[d] = true
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.kind, n.name, n.qualified, n.signature, n.start_line, n.end_line, n.language, f.path,
		       n.has_bytes, n.start_byte, n.end_byte, n.start_col, n.end_col, n.doc_start_byte, n.doc_end_byte
		FROM topology_nodes n
		JOIN topology_files f ON f.id = n.file_id
		WHERE n.kind = ?`, string(KindTest))
	if err != nil {
		return nil, fmt.Errorf("topology: tests in dirs: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var kind string
		var hasBytes int
		if scanErr := rows.Scan(&kind, &n.Name, &n.Qualified, &n.Signature,
			&n.StartLine, &n.EndLine, &n.Language, &n.Path,
			&hasBytes, &n.StartByte, &n.EndByte, &n.StartCol, &n.EndCol, &n.DocStartByte, &n.DocEndByte); scanErr != nil {
			continue
		}
		if !want[filepath.Dir(n.Path)] {
			continue
		}
		n.Kind = NodeKind(kind)
		n.HasBytes = hasBytes != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodesByKind returns every indexed node of the given kinds, with docstrings
// populated, ordered by path then start line. It is the enumeration primitive
// behind the curated structural_query checks (undocumented exports, long
// functions, …) that scan the index by symbol category rather than by name.
// Returns an empty slice when no kinds are supplied or none match.
func (s *Store) NodesByKind(ctx context.Context, kinds ...NodeKind) ([]Node, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(kinds))
	args := make([]any, len(kinds))
	for i, k := range kinds {
		placeholders[i] = "?"
		args[i] = string(k)
	}
	// G202 is a false positive here: the only thing concatenated into the SQL is
	// the "?,?,…" placeholder list; the kind values are bound parameters (args),
	// never interpolated.
	//nolint:gosec // G202: placeholders are bound-parameter markers, not user data.
	query := `
		SELECT n.kind, n.name, n.qualified, n.signature, n.docstring, n.start_line, n.end_line, n.language, f.path,
		       n.has_bytes, n.start_byte, n.end_byte, n.start_col, n.end_col, n.doc_start_byte, n.doc_end_byte
		FROM topology_nodes n
		JOIN topology_files f ON f.id = n.file_id
		WHERE n.kind IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY f.path, n.start_line`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("topology: nodes by kind: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var kind string
		var hasBytes int
		if scanErr := rows.Scan(&kind, &n.Name, &n.Qualified, &n.Signature, &n.Docstring,
			&n.StartLine, &n.EndLine, &n.Language, &n.Path,
			&hasBytes, &n.StartByte, &n.EndByte, &n.StartCol, &n.EndCol, &n.DocStartByte, &n.DocEndByte); scanErr != nil {
			continue
		}
		n.Kind = NodeKind(kind)
		n.HasBytes = hasBytes != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// ExtractFile re-parses the CURRENT content of path with the matching
// structural extractor and returns its nodes, WITHOUT touching the persisted
// index. Unlike SymbolsInFile (which reads the possibly-stale index), this
// reflects the file exactly as it is on disk right now — the property a
// symbol-edit fallback needs when the language server cannot parse the file.
// Returns (nil, nil) when no extractor handles the path.
func (s *Store) ExtractFile(ctx context.Context, path string) ([]Node, error) {
	rel := s.toRelative(paths.URIToPath(path))
	abs := rel
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.workspace, rel)
	}
	src, ex, _, _, err := s.idx.readAndHash(abs, rel)
	if err != nil {
		return nil, err
	}
	nodes, _, err := s.idx.extractFile(ctx, ex, rel, src)
	return nodes, err
}

// Explore performs a bounded BFS neighbourhood from the named symbol.
func (s *Store) Explore(ctx context.Context, name string, opts ExploreOpts) (*Neighbourhood, error) {
	return Explore(ctx, s.db, name, opts)
}

// Impact performs a bidirectional BFS around the named symbol.
func (s *Store) Impact(ctx context.Context, name string, opts ImpactOpts) (*ImpactResult, error) {
	return Impact(ctx, s.db, name, opts)
}

// ResolveNodes returns the indexed nodes matching name (optionally narrowed by
// hint), ordered deterministically. The tools use it to disambiguate a symbol
// name to a specific node before starting an ID-correct traversal.
func (s *Store) ResolveNodes(ctx context.Context, name string, hint NodeHint) ([]Node, error) {
	return ResolveNodes(ctx, s.db, name, hint)
}

// ExploreFrom performs a bounded BFS from an already-resolved centre node.
func (s *Store) ExploreFrom(ctx context.Context, centre Node, opts ExploreOpts) (*Neighbourhood, error) {
	return ExploreFrom(ctx, s.db, centre, opts)
}

// ImpactFrom performs a bidirectional BFS from an already-resolved centre node.
func (s *Store) ImpactFrom(ctx context.Context, centre Node, opts ImpactOpts) (*ImpactResult, error) {
	return ImpactFrom(ctx, s.db, centre, opts)
}

// Status returns a snapshot of the index health.
func (s *Store) Status() Status {
	return Report(s.db, s.workspace, s.idx)
}

func (s *Store) toRelative(path string) string {
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(s.workspace, path)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}
