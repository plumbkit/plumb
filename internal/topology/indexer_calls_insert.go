package topology

import (
	"database/sql"
	"fmt"
)

// insertResolvedEdges writes the edges a resolver pass produced (the Go package
// resolver and the module resolver), all with source call-resolver, so the
// scoped clear and the re-index capture/restore treat them alike.
func insertResolvedEdges(tx *sql.Tx, edges []resolvedEdge) error {
	if len(edges) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(
		`INSERT INTO topology_edges(from_id, to_id, kind, confidence, source, to_identity)
         VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("topology: resolve calls: prepare: %w", err)
	}
	defer stmt.Close()
	for _, e := range edges {
		if _, err := stmt.Exec(e.from, e.to, string(EdgeCalls), e.confidence, callResolverSource, e.toIdentity); err != nil {
			return fmt.Errorf("topology: resolve calls: insert: %w", err)
		}
	}
	return nil
}
