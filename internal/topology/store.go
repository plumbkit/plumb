package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/golimpio/plumb/internal/config"
)

// Store is the public API for topology. It owns a database connection and
// an Indexer. Obtain one via Open; release resources with Close.
//
// Concurrency: Store is safe for concurrent use after Open returns.
type Store struct {
	workspace string
	db        *sql.DB
	idx       *Indexer
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
	idx := newIndexer(workspace, db, exts, cfg.MaxFileSizeBytes)
	idx.Start()
	return &Store{workspace: workspace, db: db, idx: idx}, nil
}

// Close stops the indexer and closes the database. Safe to call from any
// goroutine; subsequent calls are no-ops.
func (s *Store) Close() error {
	s.idx.Stop()
	return s.db.Close()
}

// Enqueue schedules a file for re-indexing. path may be absolute or workspace-relative.
// Non-blocking; drops silently if the queue is full.
func (s *Store) Enqueue(path string) {
	rel := s.toRelative(path)
	s.idx.Enqueue(rel, opUpsert)
}

// EnqueueDelete schedules a file for removal from the index.
func (s *Store) EnqueueDelete(path string) {
	rel := s.toRelative(path)
	s.idx.Enqueue(rel, opDelete)
}

// Resync triggers a full workspace resync in the background.
func (s *Store) Resync() {
	s.idx.Enqueue("", opResync)
}

// Search performs a ranked FTS5 search over the topology index.
func (s *Store) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	return Search(ctx, s.db, query, opts)
}

// Explore performs a bounded BFS neighbourhood from the named symbol.
func (s *Store) Explore(ctx context.Context, name string, opts ExploreOpts) (*Neighbourhood, error) {
	return Explore(ctx, s.db, name, opts)
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
