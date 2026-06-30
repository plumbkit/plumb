package topology

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// statusReadDSN opens the topology index read-only with busy_timeout applied via
// the `_pragma=` form. The modernc driver SILENTLY IGNORES the mattn-style
// `_busy_timeout=` param (same defect fixed for stats/sessionstate), so a bare
// `_busy_timeout=2000` left this read-only status handle at busy_timeout=0 —
// turning recoverable writer contention into an immediate "database is locked"
// for topology_status / the CLI status read while the daemon holds the writer.
const statusReadDSN = "?mode=ro&_pragma=busy_timeout(2000)"

// Report builds a Status snapshot of the topology index.
func Report(db *sql.DB, workspace string, idx *Indexer) Status {
	s := Status{}
	if idx != nil {
		s.IndexerState = idx.State()
		s.LastSync = idx.LastSync()
		s.LastError = idx.LastError()
	} else {
		s.IndexerState = "stopped"
	}
	countFiles(db, &s)
	countEntities(db, &s)
	s.DBSizeBytes = dbSize(workspace)
	s.Languages = indexedLanguages(db)
	return s
}

// StatusForWorkspace opens the topology index for ws strictly read-only and
// returns a Status snapshot without starting an indexer. It is intended for
// out-of-daemon inspectors such as `plumb doctor` and the TUI. A missing
// database is reported as an error satisfying os.IsNotExist; the IndexerState in
// the returned Status is "stopped" because no live indexer is attached.
//
// The connection is opened with mode=ro so the inspection is side-effect-free:
// it never writes the main database and — when the daemon is down and the WAL
// has been checkpointed away on clean shutdown — creates no -wal/-shm sidecars.
// This mirrors stats.OpenReadOnly.
func StatusForWorkspace(ws string) (Status, error) {
	dbPath := DBPath(ws)
	if _, err := os.Stat(dbPath); err != nil {
		return Status{}, err
	}
	db, err := sql.Open("sqlite", dbPath+statusReadDSN)
	if err != nil {
		return Status{}, fmt.Errorf("topology: open db read-only: %w", err)
	}
	defer db.Close()
	return Report(db, ws, nil), nil
}

func countFiles(db *sql.DB, s *Status) {
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE error_msg = ''`).Scan(&s.IndexedFiles)
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE error_msg != ''`).Scan(&s.SkippedFiles)
	_ = db.QueryRow(`
        SELECT COUNT(*) FROM topology_files
        WHERE error_msg = '' AND (
            SELECT COUNT(*) FROM topology_nodes WHERE file_id = topology_files.id
        ) = 0`).Scan(&s.EmptyFiles)
}

func countEntities(db *sql.DB, s *Status) {
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_nodes`).Scan(&s.TotalNodes)
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_edges`).Scan(&s.TotalEdges)
}

func dbSize(workspace string) int64 {
	info, err := os.Stat(DBPath(workspace))
	if err != nil {
		return 0
	}
	return info.Size()
}

func indexedLanguages(db *sql.DB) []string {
	rows, err := db.Query(`SELECT DISTINCT language FROM topology_files WHERE language != '' AND error_msg = ''`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var langs []string
	for rows.Next() {
		var l string
		if rows.Scan(&l) == nil && l != "" {
			langs = append(langs, l)
		}
	}
	return langs
}

// FormatStatus renders a Status as a human-readable string for the topology_status tool.
func FormatStatus(s Status, workspace string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology index: %s\n", s.IndexerState)
	fmt.Fprintf(&sb, "  workspace:     %s\n", workspace)
	fmt.Fprintf(&sb, "  indexed files: %d\n", s.IndexedFiles)
	fmt.Fprintf(&sb, "  skipped files: %d\n", s.SkippedFiles)
	fmt.Fprintf(&sb, "  total nodes:   %d\n", s.TotalNodes)
	fmt.Fprintf(&sb, "  total edges:   %d\n", s.TotalEdges)
	fmt.Fprintf(&sb, "  db size:       %s\n", formatBytes(s.DBSizeBytes))
	if !s.LastSync.IsZero() {
		fmt.Fprintf(&sb, "  last sync:     %s\n", s.LastSync.Format(time.RFC3339))
	}
	if len(s.Languages) > 0 {
		fmt.Fprintf(&sb, "  languages:     %s\n", strings.Join(s.Languages, ", "))
	}
	if s.LastError != "" {
		fmt.Fprintf(&sb, "  last error:    %s\n", s.LastError)
	}
	return sb.String()
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
