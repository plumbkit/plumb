package topology

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// statusReadTimeout keeps an inspector from hanging behind the daemon's writer:
// `plumb doctor` and the TUI would rather report contention than block.
const statusReadTimeout = 2 * time.Second

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
// The connection is opened read-only, so the inspection never writes the main
// database. It may create transient -wal/-shm sidecars: reading a WAL database
// requires a WAL index, and those files are already gitignored. An earlier
// version of this comment promised no sidecars at all, which happened to be
// true only because `mode=ro` appended to a bare path is silently ignored — the
// handle was read-write, and so had no WAL index to build. This mirrors
// stats.OpenReadOnly.
func StatusForWorkspace(ws string) (Status, error) {
	dbPath := DBPath(ws)
	if _, err := os.Stat(dbPath); err != nil {
		return Status{}, err
	}
	db, err := sqlitex.OpenReadOnly(dbPath, sqlitex.ReadOnlyOptions{BusyTimeout: statusReadTimeout})
	if err != nil {
		return Status{}, fmt.Errorf("topology: open db read-only: %w", err)
	}
	defer db.Close()
	return Report(db, ws, nil), nil
}

// countFiles splits the file census by what actually happened to each row.
//
// A non-empty content_hash is the marker that a file was genuinely parsed:
// readAndHash computes one only when an extractor matched, so a recognised but
// uncovered file (and an unrecognised one) carries the empty hash that also
// makes it permanently non-stale. Counting those under "indexed" — as this did
// until the coverage work — reported a Rails app as fully indexed while its Map
// held nothing, which is the confident-wrong answer an agent cannot route
// around.
func countFiles(db *sql.DB, s *Status) {
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE error_msg = '' AND content_hash != ''`).Scan(&s.IndexedFiles)
	_ = db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE error_msg != ''`).Scan(&s.SkippedFiles)
	_ = db.QueryRow(`
        SELECT COUNT(*) FROM topology_files
        WHERE error_msg = '' AND content_hash != '' AND (
            SELECT COUNT(*) FROM topology_nodes WHERE file_id = topology_files.id
        ) = 0`).Scan(&s.EmptyFiles)
	_ = db.QueryRow(`
        SELECT COUNT(*) FROM topology_files
        WHERE error_msg = '' AND content_hash = '' AND language = ''`).Scan(&s.UnrecognisedFiles)
	s.UncoveredFiles = uncoveredCensus(db)
}

// uncoveredCensus counts, per language, the files plumb recognised but has no
// extractor for. The indexer stamps the language on those rows precisely so this
// is one aggregate query rather than a scan that resolves every path through the
// registry — Report runs on the TUI's poll, so the cost has to sit alongside the
// other COUNTs.
func uncoveredCensus(db *sql.DB) map[string]int {
	rows, err := db.Query(`
        SELECT language, COUNT(*) FROM topology_files
        WHERE error_msg = '' AND content_hash = '' AND language != ''
        GROUP BY language`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out map[string]int
	for rows.Next() {
		var lang string
		var n int
		if rows.Scan(&lang, &n) != nil || lang == "" {
			continue
		}
		if out == nil {
			out = make(map[string]int)
		}
		out[lang] = n
	}
	return out
}

// formatUncovered renders the uncovered census busiest-first, so the language
// worth wiring next leads the line. Ties break by name to keep the output
// deterministic across runs.
func formatUncovered(m map[string]int) string {
	langs := make([]string, 0, len(m))
	for l := range m {
		langs = append(langs, l)
	}
	sort.Slice(langs, func(i, j int) bool {
		if m[langs[i]] != m[langs[j]] {
			return m[langs[i]] > m[langs[j]]
		}
		return langs[i] < langs[j]
	})
	parts := make([]string, 0, len(langs))
	for _, l := range langs {
		parts = append(parts, fmt.Sprintf("%s (%d)", l, m[l]))
	}
	return strings.Join(parts, ", ")
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

// indexedLanguages lists the languages actually extracted. The content_hash
// guard keeps a recognised-but-uncovered language (whose rows carry a name but
// were never parsed) from appearing here as though its symbols were searchable.
func indexedLanguages(db *sql.DB) []string {
	rows, err := db.Query(`SELECT DISTINCT language FROM topology_files WHERE language != '' AND error_msg = '' AND content_hash != ''`)
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
	fmt.Fprintf(&sb, "  db size:       %s\n", textfmt.HumanBytes(s.DBSizeBytes))
	if !s.LastSync.IsZero() {
		fmt.Fprintf(&sb, "  last sync:     %s\n", s.LastSync.Format(time.RFC3339))
	}
	if len(s.Languages) > 0 {
		fmt.Fprintf(&sb, "  languages:     %s\n", strings.Join(s.Languages, ", "))
	}
	if len(s.UncoveredFiles) > 0 {
		fmt.Fprintf(&sb, "  not covered:   %s\n", formatUncovered(s.UncoveredFiles))
		sb.WriteString("                 (recognised languages with no extractor — these files are\n" +
			"                  listed in the index but contribute no symbols, so an empty\n" +
			"                  Map result for them is a coverage gap, not an absence)\n")
	}
	if s.UnrecognisedFiles > 0 {
		fmt.Fprintf(&sb, "  unrecognised:  %d\n", s.UnrecognisedFiles)
	}
	if s.LastError != "" {
		fmt.Fprintf(&sb, "  last error:    %s\n", s.LastError)
	}
	return sb.String()
}
