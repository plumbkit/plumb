package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"
)

const (
	importResolverSource = "import-resolver"
	importEdgeConfidence = 0.9
	// minImportSegments is the collision floor, and matchImportDir applies it at
	// both ends of a candidate: a candidate shorter than this must have had at
	// least this many segments stripped to form it. That is what keeps a local
	// strings/ directory out of every file's `import "strings"` (nothing to strip)
	// and a local http/ out of `net/http` (one bare root stripped, where a module
	// path would have spent two). Two, because the shortest Go module path is
	// host.tld/name. See matchImportDir for what it does not separate.
	minImportSegments = 2
)

func (idx *Indexer) linkImports() error {
	return idx.linkImportsContext(context.Background(), rebuildFull, indexChanges{full: true})
}

func prepareImportRebuild(ctx context.Context, tx *sql.Tx, mode rebuildMode, changed indexChanges) (string, error) {
	if mode == rebuildFull {
		if _, err := tx.ExecContext(ctx, `DELETE FROM topology_edges WHERE source = ?`, importResolverSource); err != nil {
			return "", fmt.Errorf("topology: link imports: clear: %w", err)
		}
		return "", nil
	}
	ids, err := fileIDsForPaths(ctx, tx, changed.sortedPaths())
	if err != nil {
		return "", err
	}
	if err := fillScope(ctx, tx, ids); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM topology_edges
		WHERE source = ? AND from_id IN (
			SELECT n.id FROM topology_nodes n JOIN rebuild_scope s ON s.file_id = n.file_id
		)`, importResolverSource); err != nil {
		return "", fmt.Errorf("topology: link imports: scoped clear: %w", err)
	}
	return ` AND n.file_id IN (SELECT file_id FROM rebuild_scope)`, nil
}

func (idx *Indexer) linkImportsContext(ctx context.Context, mode rebuildMode, changed indexChanges) error {
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("topology: link imports: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	where, err := prepareImportRebuild(ctx, tx, mode, changed)
	if err != nil {
		return err
	}
	pkgsByDir, err := packageNodesByDir(ctx, tx)
	if err != nil {
		return err
	}
	if len(pkgsByDir) == 0 {
		return tx.Commit()
	}
	pkgIDs := packageIDsByDir(pkgsByDir)
	//nolint:gosec // G202: where is an internal fixed SQL fragment
	rows, err := tx.QueryContext(ctx, `SELECT n.id, n.qualified
		FROM topology_nodes n
		WHERE n.kind = ? AND n.qualified <> ''`+where, string(KindImport))
	if err != nil {
		return fmt.Errorf("topology: link imports: scan imports: %w", err)
	}
	type link struct {
		from, to int64
		identity string
	}
	var links []link
	for rows.Next() {
		var id int64
		var qualified string
		if err := rows.Scan(&id, &qualified); err != nil {
			rows.Close()
			return fmt.Errorf("topology: link imports: scan: %w", err)
		}
		dir, ok := matchImportDir(qualified, pkgIDs)
		if !ok {
			continue
		}
		for _, pkg := range pkgsByDir[dir] {
			links = append(links, link{from: id, to: pkg.id, identity: pkg.identity})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("topology: link imports: rows: %w", err)
	}
	rows.Close()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO topology_edges(from_id, to_id, kind, confidence, source, to_identity)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("topology: link imports: prepare: %w", err)
	}
	defer stmt.Close()
	for _, l := range links {
		if _, err := stmt.ExecContext(ctx, l.from, l.to, string(EdgeImports), importEdgeConfidence, importResolverSource, l.identity); err != nil {
			return fmt.Errorf("topology: link imports: insert: %w", err)
		}
	}
	return tx.Commit()
}

type packageNode struct {
	id       int64
	identity string
}

func packageNodesByDir(ctx context.Context, tx *sql.Tx) (map[string][]packageNode, error) {
	rows, err := tx.QueryContext(ctx, `SELECT n.id, n.qualified, n.name, f.path
		FROM topology_nodes n JOIN topology_files f ON f.id = n.file_id
		WHERE n.kind = ?`, string(KindPackage))
	if err != nil {
		return nil, fmt.Errorf("topology: link imports: scan packages: %w", err)
	}
	defer rows.Close()
	out := map[string][]packageNode{}
	for rows.Next() {
		var id int64
		var qualified, name, p string
		if err := rows.Scan(&id, &qualified, &name, &p); err != nil {
			return nil, fmt.Errorf("topology: link imports: scan package: %w", err)
		}
		identity := p + "\x00" + qualified
		if qualified == "" {
			identity = p + "\x00" + name
		}
		out[path.Dir(p)] = append(out[path.Dir(p)], packageNode{id: id, identity: identity})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: link imports: package rows: %w", err)
	}
	return out, nil
}

func packageIDsByDir(in map[string][]packageNode) map[string][]int64 {
	out := make(map[string][]int64, len(in))
	for dir, nodes := range in {
		for _, n := range nodes {
			out[dir] = append(out[dir], n.id)
		}
	}
	return out
}

func matchImportDir(qualified string, pkgsByDir map[string][]int64) (string, bool) {
	cleaned := strings.Trim(path.Clean(strings.TrimSpace(qualified)), "/")
	if cleaned == "" || cleaned == "." {
		return "", false
	}
	segs := strings.Split(cleaned, "/")
	for start := range segs {
		// A candidate is admissible when it is long enough on its own, OR when
		// enough was stripped to form it. Both halves are the same collision rule
		// applied at the two ends, and it takes both to separate a local package
		// from a standard-library path.
		//
		// Requiring only the first half made the rule a statement about how deep the
		// INDEXED DIRECTORY sits, because pkgsByDir is keyed on the workspace-relative
		// directory: for example.com/m/stats the loop formed only example.com/m/stats
		// and m/stats, never stats, so a repository whose packages live at the top
		// level — api/, store/, cli/ — got no import edges at all.
		//
		// Requiring only the second half binds the standard library. `net/http` has a
		// segment to strip, so one stripped segment would reach a local http/, and
		// every file importing net/http would depend on it. A Go module path is at
		// minimum host.tld/name — two segments — while a stdlib path's prefix is one
		// bare root (net/, encoding/, database/, path/), so counting the stripped
		// segments is what tells them apart.
		//
		// What this does NOT separate, all of it the suffix-matching class PLAN-380
		// owns and none of it new here:
		//
		//   - a THIRD-PARTY import reaching a local package of the same name
		//     (github.com/boltdb/store → a local store/);
		//   - a stdlib path of three segments or more, whose prefix is finally long
		//     enough to pass (net/http/httptest → a local httptest/,
		//     database/sql/driver → a local driver/);
		//   - a module path of ONE dotless segment (`module myapp`), where myapp/stats
		//     is refused and such a repository keeps the no-edges behaviour it had
		//     before. Not fixed rather than newly broken.
		//
		// Resolving the module path from go.mod and requiring the import to start
		// with it settles every line above exactly, for Go. That is PLAN-380, and it
		// is the reason this pass tolerates a heuristic in the meantime: the resolver
		// is recall-biased, so an extra package in the affected set costs a test run
		// while a missing one costs a regression.
		if len(segs)-start < minImportSegments && start < minImportSegments {
			continue
		}
		cand := strings.Join(segs[start:], "/")
		if _, ok := pkgsByDir[cand]; ok {
			return cand, true
		}
	}
	return "", false
}
