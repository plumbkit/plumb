package topology

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/ignore"
)

// This file holds the indexer's whole-tree operations: deleting a single file's
// rows, and the full resync walk (filepath.WalkDir → upsert, pacing, prune of
// vanished files). See indexer.go for the worker loop, indexer_extract.go for
// per-file extraction, and indexer_persist.go for the DB writes.

// processDelete removes the rows of whatever was indexed at relPath and reports
// whether there was anything to remove. A path the index never held changes
// nothing, so it reports false and the derived-edge passes need not run for it.
func (idx *Indexer) processDelete(ctx context.Context, relPath string) error {
	_, err := idx.processDeleteChanged(ctx, relPath)
	return err
}

// processDeleteChanged removes the file indexed at relPath, or, when relPath
// names no indexed file, every file indexed below it. A directory renamed or
// removed is reported only under its own name: no watcher backend reports the
// files it held, so without the subtree its old children would stay indexed
// until the next resync, and a resync is suppressed while the watcher runs.
// It is called only once relPath is gone (or escapes the workspace), so
// nothing below it can still be live.
func (idx *Indexer) processDeleteChanged(ctx context.Context, relPath string) (bool, error) {
	_ = ctx
	ids, err := idx.indexedAtOrBelow(relPath)
	if err != nil || len(ids) == 0 {
		return false, err
	}
	tx, err := idx.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeded; on the failure path the error is already being returned
	for _, id := range ids {
		if err := deleteFileNodes(tx, id); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DELETE FROM topology_files WHERE id = ?`, id); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// indexedAtOrBelow returns the id of the file indexed at relPath or, failing
// that, the ids of every file indexed below it. The subtree is a half-open range
// over the path index, [relPath+sep, relPath+(sep+1)), never a LIKE: a name may
// hold % or _, and a sibling sharing the prefix (a.go, ab/) sorts outside it.
func (idx *Indexer) indexedAtOrBelow(relPath string) ([]int64, error) {
	var id int64
	err := idx.db.QueryRow(`SELECT id FROM topology_files WHERE path = ?`, relPath).Scan(&id)
	if err == nil {
		return []int64{id}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if relPath == "" || relPath == "." {
		return nil, nil // the root itself is never deleted file by file
	}
	rows, err := idx.db.Query(`SELECT id FROM topology_files WHERE path >= ? AND path < ?`,
		relPath+string(filepath.Separator), relPath+string(filepath.Separator+1))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// errResyncAborted is returned through filepath.Walk when the indexer is
// stopping (or its context is cancelled) mid-resync, so processResync can skip
// pruning — a partial walk must not delete files it simply hasn't visited yet.
var errResyncAborted = errors.New("topology: resync aborted")

// processResync walks the whole tree and reports whether anything changed.
// A periodic resync over an untouched tree is the common case on a live daemon,
// and reporting false for it is what keeps the derived-edge passes from
// rebuilding a graph nothing has disturbed.
func (idx *Indexer) processResync(ctx context.Context) error {
	_, err := idx.processResyncChanged(ctx)
	return err
}

func (idx *Indexer) processResyncChanged(ctx context.Context) (bool, error) {
	present := make(map[string]bool)
	processed := 0
	changed := false
	root := filepath.Clean(idx.workspace)
	// One ignore.Stack per directory, keyed by absolute path. WalkDir is
	// depth-first and pre-order, so a directory's parent is always loaded before
	// it, and a pruned directory never has children that look it up.
	//
	// The walk PRUNES what it excludes rather than filtering it, which is what
	// ignore.Stack.IsIgnored is contracted for: a file under an excluded
	// directory usually matches no rule of its own, so filtering would both pay
	// the descent and get the answer wrong.
	stacks := make(map[string]ignore.Stack)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		// Abort promptly on shutdown or context cancellation, independent of
		// pacing. pace() is the only other place the walk observes idx.done/ctx,
		// and it is a no-op when pacing is disabled (resyncBatch/resyncPause == 0),
		// so without this unconditional check a pacing-off resync would run to
		// completion and block Stop()'s wg.Wait() for the whole walk of a large
		// workspace. See #65.
		select {
		case <-idx.done:
			return errResyncAborted
		case <-ctx.Done():
			return errResyncAborted
		default:
		}
		if walkErr != nil {
			// Surface permission errors and other walk failures in the last-error field
			// rather than silently swallowing them.
			slog.Warn("topology: resync walk error", "path", path, "err", walkErr)
			return nil
		}
		if d.IsDir() {
			return idx.resyncEnterDir(root, path, d.Name(), stacks)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if idx.resyncSkipsFile(path, rel, stacks) {
			return nil
		}
		present[rel] = true
		fileChanged, upErr := idx.processUpsertChanged(ctx, rel)
		if upErr != nil {
			return upErr
		}
		changed = changed || fileChanged
		processed++
		return idx.pace(ctx, processed)
	})
	if errors.Is(err, errResyncAborted) {
		// Shutting down: skip prune so a partial walk cannot delete live files.
		return changed, nil
	}
	if err != nil {
		return changed, fmt.Errorf("topology: resync walk: %w", err)
	}
	pruned, err := idx.pruneDeletedChanged(present)
	if err != nil {
		return changed || pruned, err
	}
	changed = changed || pruned
	// A full resync builds a large transient working set (file reads, parse
	// trees, node/edge slices). Release the pooled parse arena to the GC, then
	// hand the freed pages back to the OS so RSS and HeapSys settle to steady
	// state instead of lingering at the walk's peak.
	idx.reclaimFn()
	return changed, nil
}

// resyncEnterDir decides what the resync walk does with one directory: prune it
// (fs.SkipDir) or record the ignore rules in force inside it. Split out of the
// walk closure so that closure stays readable.
//
// The three exclusions are additive and ordered cheapest-first: the hardcoded
// floor, then the configured patterns, then the tree's own ignore files.
func (idx *Indexer) resyncEnterDir(root, path, name string, stacks map[string]ignore.Stack) error {
	if path == root {
		// The workspace root is never judged by the skip list. It used to be: a
		// checkout living at ~/.config/repo or ~/src/build had its own name
		// matched by shouldSkipDir and indexed nothing.
		var st ignore.Stack
		stacks[root] = st.Load(root)
		return nil
	}
	if shouldSkipDir(name) {
		return fs.SkipDir
	}
	rel, relErr := filepath.Rel(root, path)
	if relErr != nil {
		return fs.SkipDir
	}
	if matchesExcludePattern(idx.excludePatterns, rel) {
		return fs.SkipDir
	}
	parent := stacks[filepath.Dir(path)]
	if parent.IsIgnored(path, true) {
		return fs.SkipDir
	}
	stacks[path] = parent.Load(path)
	return nil
}

// resyncSkipsFile reports whether the walk excludes one file. Its directory's
// stack is already loaded, because a pruned directory never reaches here.
func (idx *Indexer) resyncSkipsFile(path, rel string, stacks map[string]ignore.Stack) bool {
	if matchesExcludePattern(idx.excludePatterns, rel) {
		return true
	}
	return stacks[filepath.Dir(path)].IsIgnored(path, false)
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

// pruneDeleted removes rows for files that have vanished from the tree and
// reports whether it removed any.
func (idx *Indexer) pruneDeleted(present map[string]bool) error {
	_, err := idx.pruneDeletedChanged(present)
	return err
}

func (idx *Indexer) pruneDeletedChanged(present map[string]bool) (bool, error) {
	rows, err := idx.db.Query(`SELECT id, path FROM topology_files`)
	if err != nil {
		return false, err
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
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		return false, fmt.Errorf("topology: prune scan: %w", scanErr)
	}
	var firstErr error
	removed := false
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
		if err := tx.Commit(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("topology: prune commit for %q: %w", e.path, err)
			}
			continue
		}
		removed = true
	}
	return removed, firstErr
}

// shouldSkipDir returns true for directories that should never be indexed.
// Dot-prefixed directories (hidden dirs like .vscode, .idea, .venv) are always
// skipped to avoid indexing editor artefacts and virtual environments.
//
// This is the FLOOR, not the whole rule. The resync walk additionally honours
// .gitignore / .ignore and [topology] exclude_patterns, both of which can only
// exclude more. A repository that tracks its own vendor/ tree still does not
// get it indexed, and a workspace with no ignore file behaves exactly as it did
// before the walk learned to read them.
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
