package history

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// PruneResult counts what Prune removed.
type PruneResult struct{ Changes, Paths, Workspaces int64 }

// pruneChunk bounds how many changes one prune transaction deletes, so the
// daemon's writer — waiting on the same lock under its busy timeout — gets a
// turn between chunks instead of stalling behind one long DELETE.
const pruneChunk = 2000

// Prune deletes changes older than before (optionally one workspace's), then
// garbage-collects unreferenced paths (path_id AND from_path_id) and workspaces.
// It is a second writer beside a running daemon: short transactions under the
// sqlitex busy timeout are safe in WAL mode. It never VACUUMs (see Vacuum).
func Prune(path string, before time.Time, workspace string) (PruneResult, error) {
	db, err := sqlitex.Open(path, sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		return PruneResult{}, fmt.Errorf("history: open for prune: %w", err)
	}
	defer db.Close()
	if err := initSchema(db); err != nil {
		return PruneResult{}, err
	}
	return prune(db, before, workspace, pruneChunk)
}

// prune is Prune on an open, initialised database, deleting changes chunk rows
// per autocommit statement.
func prune(db *sql.DB, before time.Time, workspace string, chunk int) (PruneResult, error) {
	var res PruneResult
	sel := `SELECT seq FROM changes WHERE ts_ms < ?`
	args := []any{before.UnixMilli()}
	if workspace != "" {
		sel += ` AND workspace_id = (SELECT id FROM workspaces WHERE root = ?)`
		args = append(args, paths.Canonical(workspace))
	}
	q := `DELETE FROM changes WHERE seq IN (` + sel + ` ORDER BY seq LIMIT ?)`
	args = append(args, chunk)
	for {
		r, err := db.Exec(q, args...)
		if err != nil {
			return res, fmt.Errorf("history: prune: %w", err)
		}
		n, _ := r.RowsAffected()
		res.Changes += n
		if n < int64(chunk) {
			break
		}
	}
	for _, step := range []struct {
		n   *int64
		sql string
	}{
		{&res.Paths, `DELETE FROM paths WHERE id NOT IN (SELECT path_id FROM changes
			UNION SELECT from_path_id FROM changes WHERE from_path_id IS NOT NULL)`},
		{&res.Workspaces, `DELETE FROM workspaces WHERE id NOT IN (SELECT workspace_id FROM paths)`},
	} {
		r, err := db.Exec(step.sql)
		if err != nil {
			return res, fmt.Errorf("history: prune: %w", err)
		}
		*step.n, _ = r.RowsAffected()
	}
	return res, nil
}

// Vacuum reclaims file space. It needs an exclusive lock: the caller must have
// established the daemon is not running.
func Vacuum(path string) error {
	db, err := sqlitex.Open(path, sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		return fmt.Errorf("history: open for vacuum: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("history: vacuum: %w", err)
	}
	return nil
}
