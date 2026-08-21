package topology

import (
	"database/sql"
	"fmt"
	"path"
	"strings"
)

// importResolverSource marks edges this pass owns. They are DERIVED data: the
// pass deletes and rebuilds every edge carrying this source, so it must never be
// used by an extractor.
const importResolverSource = "import-resolver"

// importEdgeConfidence is below an extractor's 1.0 because the link is inferred
// from a path suffix rather than read from a parsed reference.
const importEdgeConfidence = 0.9

// minImportSegments is the shortest suffix an import path may match on.
//
// Requiring two segments is what keeps stdlib and third-party imports out. A Go
// file importing "strings" must not be linked to a local directory that happens
// to be called strings/ — and single-segment names are exactly where collisions
// live: this workspace holds 636 nodes named "strings". Two segments
// ("internal/stats") is specific enough that a false match needs a genuinely
// coincidental layout, and module-internal imports always have at least that
// much once the module prefix is stripped.
const minImportSegments = 2

// linkImports creates the index's only cross-file edges.
//
// Extractors run per file and emit edges as indices into that file's own node
// slice (see insertEdges), so nothing they produce can leave the file. The
// consequence was measurable and severe: before this pass the index held ~31k
// edges and NOT ONE of them crossed a file boundary. Since a Go test never lives
// in the file it exercises, "affected by a dependency edge" could not fire for
// test selection at all, and topology_affected was in practice a same-directory
// test finder wearing a dependency-graph description.
//
// The link is import node -> package node, and the direction matters: callers
// ask "who depends on the thing I changed?", which is an inward traversal, so
// the edge must point AT the package being imported.
//
// An import is linked to every package node in the target directory, not one:
// package nodes are per-file, and importing a package depends on all of it, so a
// change to any file in it should reach the importer.
func (idx *Indexer) linkImports() error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("topology: link imports: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Derived edges are rebuilt wholesale. Incremental correctness is why: when a
	// file is re-indexed its nodes are deleted, and ON DELETE CASCADE takes with
	// them both the edges out of its imports AND the edges other files' imports
	// pointed at its package node. Patching just the changed file would leave the
	// second kind missing until something else touched the importer.
	if _, err := tx.Exec(`DELETE FROM topology_edges WHERE source = ?`, importResolverSource); err != nil {
		return fmt.Errorf("topology: link imports: clear: %w", err)
	}

	pkgsByDir, err := packageNodesByDir(tx)
	if err != nil {
		return err
	}
	if len(pkgsByDir) == 0 {
		return tx.Commit()
	}

	rows, err := tx.Query(
		`SELECT n.id, n.qualified
           FROM topology_nodes n
          WHERE n.kind = ? AND n.qualified <> ''`, string(KindImport))
	if err != nil {
		return fmt.Errorf("topology: link imports: scan imports: %w", err)
	}
	type link struct {
		from int64
		to   int64
	}
	var links []link
	for rows.Next() {
		var id int64
		var qualified string
		if err := rows.Scan(&id, &qualified); err != nil {
			rows.Close()
			return fmt.Errorf("topology: link imports: scan: %w", err)
		}
		dir, ok := matchImportDir(qualified, pkgsByDir)
		if !ok {
			continue
		}
		for _, pkgID := range pkgsByDir[dir] {
			links = append(links, link{from: id, to: pkgID})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("topology: link imports: rows: %w", err)
	}
	rows.Close()

	stmt, err := tx.Prepare(
		`INSERT INTO topology_edges(from_id, to_id, kind, confidence, source)
         VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("topology: link imports: prepare: %w", err)
	}
	defer stmt.Close()
	for _, l := range links {
		if _, err := stmt.Exec(l.from, l.to, string(EdgeImports), importEdgeConfidence, importResolverSource); err != nil {
			return fmt.Errorf("topology: link imports: insert: %w", err)
		}
	}
	return tx.Commit()
}

// packageNodesByDir groups every package node by the directory of its file.
// Package nodes are per-file, so a directory maps to as many ids as it has
// indexed source files.
func packageNodesByDir(tx *sql.Tx) (map[string][]int64, error) {
	rows, err := tx.Query(
		`SELECT n.id, f.path
           FROM topology_nodes n
           JOIN topology_files f ON f.id = n.file_id
          WHERE n.kind = ?`, string(KindPackage))
	if err != nil {
		return nil, fmt.Errorf("topology: link imports: scan packages: %w", err)
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, fmt.Errorf("topology: link imports: scan package: %w", err)
		}
		out[path.Dir(p)] = append(out[path.Dir(p)], id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: link imports: package rows: %w", err)
	}
	return out, nil
}

// matchImportDir maps an import path to an indexed directory by longest suffix.
//
// Deliberately not go.mod-aware. The module prefix is exactly the part that
// differs per language and per project, while the tail is the repository-relative
// path in Go, Python and TypeScript alike, so matching the longest suffix that
// names a real indexed directory generalises without teaching this pass any one
// build system. "github.com/plumbkit/plumb/internal/stats" and "internal/stats"
// both land on internal/stats; "strings" and "github.com/spf13/cobra" match
// nothing and are skipped.
func matchImportDir(qualified string, pkgsByDir map[string][]int64) (string, bool) {
	cleaned := strings.Trim(path.Clean(strings.TrimSpace(qualified)), "/")
	if cleaned == "" || cleaned == "." {
		return "", false
	}
	segs := strings.Split(cleaned, "/")
	// Longest suffix first: a deeper match is the more specific one.
	for start := 0; start+minImportSegments <= len(segs); start++ {
		cand := strings.Join(segs[start:], "/")
		if _, ok := pkgsByDir[cand]; ok {
			return cand, true
		}
	}
	return "", false
}
