package topology

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	tsg "github.com/odvcencio/gotreesitter"
)

// This file holds the indexer's whole-tree operations: deleting a single file's
// rows, and the full resync walk (filepath.Walk → upsert, pacing, prune of
// vanished files). See indexer.go for the worker loop, indexer_extract.go for
// per-file extraction, and indexer_persist.go for the DB writes.

func (idx *Indexer) processDelete(ctx context.Context, relPath string) error {
	_ = ctx
	var fileID int64
	row := idx.db.QueryRow(`SELECT id FROM topology_files WHERE path = ?`, relPath)
	if err := row.Scan(&fileID); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := deleteFileNodes(tx, fileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM topology_files WHERE id = ?`, fileID); err != nil {
		return err
	}
	return tx.Commit()
}

// errResyncAborted is returned through filepath.Walk when the indexer is
// stopping (or its context is cancelled) mid-resync, so processResync can skip
// pruning — a partial walk must not delete files it simply hasn't visited yet.
var errResyncAborted = errors.New("topology: resync aborted")

func (idx *Indexer) processResync(ctx context.Context) error {
	present := make(map[string]bool)
	processed := 0
	err := filepath.Walk(idx.workspace, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Surface permission errors and other walk failures in the last-error field
			// rather than silently swallowing them.
			slog.Warn("topology: resync walk error", "path", path, "err", walkErr)
			return nil
		}
		if info.IsDir() {
			if shouldSkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(idx.workspace, path)
		if relErr != nil {
			return nil
		}
		present[rel] = true
		if upErr := idx.processUpsert(ctx, rel); upErr != nil {
			return upErr
		}
		processed++
		return idx.pace(ctx, processed)
	})
	if errors.Is(err, errResyncAborted) {
		// Shutting down: skip prune so a partial walk cannot delete live files.
		return nil
	}
	if err != nil {
		return fmt.Errorf("topology: resync walk: %w", err)
	}
	if err := idx.pruneDeleted(present); err != nil {
		return err
	}
	// A full resync builds a large transient working set (file reads, parse
	// trees, node/edge slices). Release the pooled parse arena to the GC, then
	// hand the freed pages back to the OS so RSS and HeapSys settle to steady
	// state instead of lingering at the walk's peak.
	tsg.DrainArenaPools()
	debug.FreeOSMemory()
	return nil
}

// pace throttles the full resync walk: after every resyncBatch files it pauses
// for resyncPause, yielding CPU to live tool calls and other workspaces sharing
// the daemon. It returns errResyncAborted when the indexer is stopping or the
// context is cancelled. A zero batch or pause disables pacing entirely.
func (idx *Indexer) pace(ctx context.Context, processed int) error {
	if idx.resyncBatch <= 0 || idx.resyncPause <= 0 || processed%idx.resyncBatch != 0 {
		return nil
	}
	select {
	case <-idx.done:
		return errResyncAborted
	case <-ctx.Done():
		return errResyncAborted
	case <-time.After(idx.resyncPause):
		return nil
	}
}

func (idx *Indexer) pruneDeleted(present map[string]bool) error {
	rows, err := idx.db.Query(`SELECT id, path FROM topology_files`)
	if err != nil {
		return err
	}
	type entry struct {
		id   int64
		path string
	}
	var stale []entry
	for rows.Next() {
		var e entry
		if rows.Scan(&e.id, &e.path) == nil && !present[e.path] {
			stale = append(stale, e)
		}
	}
	rows.Close()
	var firstErr error
	for _, e := range stale {
		tx, txErr := idx.db.Begin()
		if txErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("topology: prune begin tx for %q: %w", e.path, txErr)
			}
			continue
		}
		if err := deleteFileNodes(tx, e.id); err != nil {
			_ = tx.Rollback()
			if firstErr == nil {
				firstErr = fmt.Errorf("topology: prune nodes for %q: %w", e.path, err)
			}
			continue
		}
		if _, err := tx.Exec(`DELETE FROM topology_files WHERE id = ?`, e.id); err != nil {
			_ = tx.Rollback()
			if firstErr == nil {
				firstErr = fmt.Errorf("topology: prune file %q: %w", e.path, err)
			}
			continue
		}
		if err := tx.Commit(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("topology: prune commit for %q: %w", e.path, err)
		}
	}
	return firstErr
}

// shouldSkipDir returns true for directories that should never be indexed.
// Dot-prefixed directories (hidden dirs like .vscode, .idea, .venv) are always
// skipped to avoid indexing editor artefacts and virtual environments.
func shouldSkipDir(name string) bool {
	if len(name) > 1 && name[0] == '.' {
		return true
	}
	switch name {
	case "vendor", "node_modules", "testdata", "dist", "build", "__pycache__":
		return true
	}
	return false
}
