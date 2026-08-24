package topology

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"sort"
)

// This file holds the LIFECYCLE for the index's derived cross-file edges — the
// `import-resolver` and `call-resolver` sources.
// Historical pre-PLAN-372 baseline: both passes deleted and rebuilt every edge
// on every queue, costing ~0.9s and ~8 MB of WAL for a one-character save on plumb's own
// tree. The current lifecycle preserves incoming edges and scopes caller rebuilds.
//
// The hazard the whole design is shaped around: topology_edges CASCADEs on BOTH
// endpoints (db.go), so re-indexing file F silently deletes the edges out of F's
// nodes AND the edges other files' nodes pointed AT. A derived edge is therefore
// never durable data — it is a projection of durable data, and the durable data
// is addressed by PATH and NAME (topology_files.path, topology_call_sites'
// TEXT callee/qualifier, an import node's qualified path), never by a node
// rowid. Rebuilding a scope means: delete the projection for that scope, then
// re-derive it from the text. Nothing in this file may key a cross-file edge on
// a rowid that outlives the transaction that read it.

// indexChanges records what one queue cycle actually changed, as opposed to what
// it was asked to do. A save that rewrites a file with identical bytes reaches
// the indexer as an upsert and changes nothing, and a derived-edge pass over an
// unchanged graph is pure cost — so "did anything change" is the first gate.
type indexChanges struct {
	// full is set when a whole-tree resync changed something. A resync can add,
	// remove and move files in one drain, so its scope is not usefully narrower
	// than the whole index.
	full bool
	// paths are workspace-relative paths whose rows changed.
	paths map[string]struct{}
}

func (c *indexChanges) markFull() { c.full = true }

func (c *indexChanges) mark(relPath string) {
	if c.paths == nil {
		c.paths = make(map[string]struct{})
	}
	c.paths[relPath] = struct{}{}
}

func (c *indexChanges) none() bool { return !c.full && len(c.paths) == 0 }

// sortedPaths returns the changed paths in a stable order, so a rebuild's SQL
// argument order does not depend on map iteration.
func (c *indexChanges) sortedPaths() []string {
	out := make([]string, 0, len(c.paths))
	for p := range c.paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// rebuildMode is what a derived-edge pass has been asked to do this cycle.
type rebuildMode int

const (
	// rebuildSkip: nothing that could change a derived edge changed.
	rebuildSkip rebuildMode = iota
	// rebuildScoped: rebuild only the edges the changed files can have affected.
	rebuildScoped
	// rebuildFull: delete and re-derive every edge the pass owns.
	rebuildFull
)

// metaResolverSurface stores the stable identities that can change a derived
// target without changing any caller file: resolver-eligible package nodes and
// exported top-level Go functions. Rowids and bodies are deliberately absent, so
// an ordinary body-only re-index remains scoped and preserves the <1s lifecycle.
const metaResolverSurface = "topology.resolver_surface"

// planRebuild decides what this cycle's derived-edge passes must do, and returns
// the fingerprint to persist once they have both succeeded.
//
// A missing fingerprint means "this index was built by a version that had no
// lifecycle", so its derived edges cannot be assumed complete: full rebuild.
// That is also what makes the skip path safe — a skip is only ever taken when a
// previous pass recorded a fingerprint, i.e. when the edges are known to have
// been built by this code.
func (idx *Indexer) planRebuild(ctx context.Context, c indexChanges) (rebuildMode, string, error) {
	stored, ok, err := readMeta(ctx, idx.db, metaResolverSurface)
	if err != nil {
		return rebuildFull, "", err
	}
	if !ok || idx.forceFullRebuild || c.full {
		fp, fpErr := resolverSurfaceFingerprint(ctx, idx.db)
		return rebuildFull, fp, fpErr
	}
	if c.none() {
		return rebuildSkip, stored, nil
	}
	fp, err := resolverSurfaceFingerprint(ctx, idx.db)
	if err != nil {
		return rebuildFull, "", err
	}
	if fp != stored {
		return rebuildFull, fp, nil
	}
	return rebuildScoped, fp, nil
}

// resolverSurfaceFingerprint hashes stable identities for every resolver target
// surface. Package nodes affect import resolution for every language; exported
// top-level Go functions affect the call resolver. File paths plus qualified/name
// are identity, while rowids, bodies, signatures and other mutable fields are not.
func resolverSurfaceFingerprint(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT n.kind, n.language, f.path, n.qualified, n.name
           FROM topology_nodes n
           JOIN topology_files f ON f.id = n.file_id
          WHERE n.kind = ? OR (n.kind = ? AND n.language = 'go')`,
		string(KindPackage), string(KindFunction))
	if err != nil {
		return "", fmt.Errorf("topology: resolver surface fingerprint: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]struct{})
	for rows.Next() {
		var kind, lang, p, qualified, name string
		if err := rows.Scan(&kind, &lang, &p, &qualified, &name); err != nil {
			return "", fmt.Errorf("topology: resolver surface fingerprint scan: %w", err)
		}
		if kind == string(KindFunction) && !isExportedName(name) {
			continue
		}
		identity := qualified
		if identity == "" {
			identity = name
		}
		seen[kind+"\x00"+lang+"\x00"+p+"\x00"+identity] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("topology: resolver surface fingerprint rows: %w", err)
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := fnv.New64a()
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{'\n'})
	}
	return fmt.Sprintf("%d:%016x", len(keys), h.Sum64()), nil
}

func readMeta(ctx context.Context, db *sql.DB, key string) (string, bool, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM topology_meta WHERE key = ?`, key).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("topology: read meta %s: %w", key, err)
	}
	return v, true, nil
}

func writeMeta(ctx context.Context, db *sql.DB, value string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO topology_meta(key, value) VALUES (?, ?)
         ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaResolverSurface, value)
	if err != nil {
		return fmt.Errorf("topology: write meta %s: %w", metaResolverSurface, err)
	}
	return nil
}

// fileIDsForPaths resolves changed paths to file ids, skipping paths whose row
// is gone (a deleted file). A deleted file's rows — and every edge that
// CASCADEd off them — are already absent, so it needs no scope entry of its own;
// what it still needs is its DIRECTORY, which is why the callers derive scope
// from paths and not from ids alone.
func fileIDsForPaths(ctx context.Context, tx *sql.Tx, paths []string) ([]int64, error) {
	out := make([]int64, 0, len(paths))
	for _, p := range paths {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM topology_files WHERE path = ?`, p).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			continue
		case err != nil:
			return nil, fmt.Errorf("topology: scope file id: %w", err)
		}
		out = append(out, id)
	}
	return out, nil
}

// scopeTable is the connection-local table a scoped rebuild joins against.
//
// It exists instead of an inlined IN (?, ?, …) list because the scope of a call
// rebuild is not the changed files but every file that imports their
// directories, which on a widely-imported package is hundreds of files — past
// the point where a parameter list stays readable, and eventually past SQLite's
// variable limit. It is a TEMP table, so it lives on the one pooled connection
// the enclosing transaction holds; the indexer's writes all run on a single
// background goroutine, so no second writer can be inside one of these
// transactions at the same time.
const scopeTable = "temp.rebuild_scope"

func fillScope(ctx context.Context, tx *sql.Tx, ids []int64) error {
	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE IF NOT EXISTS rebuild_scope(file_id INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("topology: create scope: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+scopeTable); err != nil {
		return fmt.Errorf("topology: clear scope: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO `+scopeTable+`(file_id) VALUES (?)`)
	if err != nil {
		return fmt.Errorf("topology: prepare scope: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("topology: fill scope: %w", err)
		}
	}
	return nil
}
