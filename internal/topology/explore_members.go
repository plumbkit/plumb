package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
)

func isTypeKind(k NodeKind) bool {
	return k == KindType || k == KindClass
}

func fetchContainedNodeIDs(ctx context.Context, db *sql.DB, centreID int64) (map[int64]bool, error) {
	containedRows, err := db.QueryContext(ctx,
		`SELECT to_id FROM topology_edges WHERE from_id = ? AND kind = 'contains'`,
		centreID)
	if err != nil {
		return nil, fmt.Errorf("topology: type members contains query: %w", err)
	}
	defer containedRows.Close()

	containedIDs := make(map[int64]bool)
	for containedRows.Next() {
		var toID int64
		if err := containedRows.Scan(&toID); err != nil {
			return nil, fmt.Errorf("topology: scan contained id: %w", err)
		}
		containedIDs[toID] = true
	}
	if err := containedRows.Err(); err != nil {
		return nil, fmt.Errorf("topology: contained rows iteration: %w", err)
	}
	return containedIDs, nil
}

func isSamePackageReceiverMethod(m Node, centrePath, centreDir, cleanName string) bool {
	if centrePath != "" && filepath.Dir(m.Path) != centreDir {
		return false
	}
	recv, _, ok := GoMethodReceiver(m.Qualified)
	return ok && StripTypeParams(recv) == cleanName
}

// TypeMembers returns the indexed member symbols (methods, fields) belonging to
// a type/class/interface node, bounded up to limit. It joins both explicit
// EdgeContains edges (common in Python, TS, Java) and same-package receiver methods
// (Go conventions where methods share the package directory with the type).
func TypeMembers(ctx context.Context, db *sql.DB, centre Node, limit int) ([]Node, error) {
	if limit <= 0 {
		limit = 50
	}
	if !isTypeKind(centre.Kind) {
		return nil, nil
	}

	centreDir := filepath.Dir(centre.Path)
	cleanName := StripTypeParams(centre.Name)

	containedIDs, err := fetchContainedNodeIDs(ctx, db, centre.ID)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT n.id, n.file_id, n.kind, n.name, n.qualified, n.signature,
                n.start_line, n.end_line, n.docstring, n.language, f.path
         FROM topology_nodes n
         JOIN topology_files f ON f.id = n.file_id
         WHERE (
             n.id IN (
                 SELECT to_id FROM topology_edges
                 WHERE from_id = ? AND kind = 'contains'
             )
             OR (
                 n.kind = 'method'
                 AND (
                     n.qualified = '(*' || ? || ').' || n.name
                     OR n.qualified = '(' || ? || ').' || n.name
                     OR n.qualified LIKE '(*' || ? || '[%).' || n.name
                     OR n.qualified LIKE '(' || ? || '[%).' || n.name
                 )
                 AND (
                     f.id = ?
                     OR (? != '.' AND f.path LIKE ? || '/%')
                     OR (? = '.' AND f.path NOT LIKE '%/%')
                 )
             )
         )
         AND n.kind IN ('method', 'function', 'field')
         ORDER BY f.path, n.start_line`,
		centre.ID,
		cleanName, cleanName, cleanName, cleanName,
		centre.FileID, centreDir, centreDir, centreDir)
	if err != nil {
		return nil, fmt.Errorf("topology: type members query: %w", err)
	}
	defer rows.Close()

	var members []Node
	for rows.Next() {
		var m Node
		if err := rows.Scan(&m.ID, &m.FileID, &m.Kind, &m.Name, &m.Qualified, &m.Signature,
			&m.StartLine, &m.EndLine, &m.Docstring, &m.Language, &m.Path); err != nil {
			return nil, fmt.Errorf("topology: scan type member: %w", err)
		}

		if !containedIDs[m.ID] && !isSamePackageReceiverMethod(m, centre.Path, centreDir, cleanName) {
			continue
		}

		members = append(members, m)
		if len(members) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: type members iteration: %w", err)
	}
	return members, nil
}
