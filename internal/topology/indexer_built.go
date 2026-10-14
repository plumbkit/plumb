package topology

import (
	"context"
	"database/sql"
	"log/slog"
)

// indexer_built.go — whether this index has ever completed a full pass.
//
// A first build of a large workspace takes a minute or more, and until it ends the
// index holds part of the tree only: an answer read from it looks like a complete
// one. LastSync cannot say so, because it lives in memory: after every daemon
// restart a complete, persisted index would look unbuilt until its first cycle
// ended. So the fact is persisted, written when a full resync first succeeds and
// read back on open. A schema rebuild drops topology_meta with every other table,
// so a rebuilt index is unbuilt again, as it is.

const metaBuilt = "topology.built"

// loadBuilt reports whether db records a completed full pass. A read error counts
// as not built: the cost is a "still being built" label, never a false "complete".
func loadBuilt(ctx context.Context, db *sql.DB) bool {
	_, ok, err := readMeta(ctx, db, metaBuilt)
	return err == nil && ok
}

// markBuilt records that a full pass has completed, once per index. The marker is
// set in memory even when the write fails, so this process stops saying the index
// is being built; only a later process would say it again, until its own first pass.
func (idx *Indexer) markBuilt(ctx context.Context) {
	idx.mu.RLock()
	done := idx.built
	idx.mu.RUnlock()
	if done {
		return
	}
	if _, err := idx.db.ExecContext(ctx,
		`INSERT INTO topology_meta(key, value) VALUES (?, '1')
         ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaBuilt); err != nil {
		slog.Warn("topology: could not record the completed full pass", "workspace", idx.workspace, "err", err)
	}
	idx.mu.Lock()
	idx.built = true
	idx.mu.Unlock()
}

// IndexedFileCount is how many files the index has parsed so far: Status's
// IndexedFiles without the rest of its census, cheap enough for every call that
// must say how far a first build has got.
func (s *Store) IndexedFileCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM topology_files WHERE error_msg = '' AND content_hash != ''`).Scan(&n)
	return n, err
}
