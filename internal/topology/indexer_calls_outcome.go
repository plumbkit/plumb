package topology

import (
	"context"
	"database/sql"
	"fmt"
)

// indexer_calls_outcome.go — each call site's resolver outcome, kept on its row so
// a reader can tell the calls of a name that resolved from the ones still open
// (callsites_query.go, OpenCallSites).

// siteOutcome is one site's bucket, recorded on the site after the pass.
type siteOutcome struct {
	id     int64
	bucket resolveBucket
}

// writeSiteOutcomes stores each site's outcome on its row, after the scan has
// closed, so a reader can ask which calls of a name are still open
// (OpenCallSites). A scoped pass writes only the sites it read, as it writes only
// their edges.
func writeSiteOutcomes(ctx context.Context, tx *sql.Tx, outcomes []siteOutcome) error {
	if len(outcomes) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `UPDATE topology_call_sites SET outcome = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("topology: resolve calls: prepare outcome: %w", err)
	}
	defer stmt.Close()
	for _, o := range outcomes {
		if _, err := stmt.ExecContext(ctx, int(o.bucket), o.id); err != nil {
			return fmt.Errorf("topology: resolve calls: outcome: %w", err)
		}
	}
	return nil
}
