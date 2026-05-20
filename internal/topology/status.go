package topology

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

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

func countFiles(db *sql.DB, s *Status) {
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE error_msg = ''`).Scan(&s.IndexedFiles)
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE error_msg != ''`).Scan(&s.SkippedFiles)
	_ = db.QueryRow(`
        SELECT COUNT(*) FROM topology_files
        WHERE error_msg = '' AND (
            SELECT COUNT(*) FROM topology_nodes WHERE file_id = topology_files.id
        ) = 0`).Scan(&s.StaleFiles)
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
