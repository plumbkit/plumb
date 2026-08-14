package topology

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReport_EmptyDB(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "status.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	s := Report(db, dir, nil)
	if s.IndexerState != "stopped" {
		t.Errorf("state = %q, want stopped", s.IndexerState)
	}
	if s.IndexedFiles != 0 {
		t.Errorf("IndexedFiles = %d, want 0", s.IndexedFiles)
	}
	if s.TotalNodes != 0 {
		t.Errorf("TotalNodes = %d, want 0", s.TotalNodes)
	}
}

func TestReport_WithData(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "x.go")
	insertTestNode(t, db, fileID, "x.go", Node{Kind: KindFunction, Name: "X", Language: "go"})

	s := Report(db, dir, nil)
	if s.IndexedFiles != 1 {
		t.Errorf("IndexedFiles = %d, want 1", s.IndexedFiles)
	}
	if s.TotalNodes != 1 {
		t.Errorf("TotalNodes = %d, want 1", s.TotalNodes)
	}
}

func TestIndexedLanguages(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "langs.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// A parsed row always carries a content hash; readAndHash computes one only
	// when an extractor matched. The uncovered row below is the counter-case: a
	// named language with no hash, which must NOT read as an indexed language.
	insertLangFixture(t, db, "a.go", "go", "h1", "")
	insertLangFixture(t, db, "b.py", "python", "h2", "")
	insertLangFixture(t, db, "c.go", "go", "h3", "")
	insertLangFixture(t, db, "err.go", "", "", "oops")
	insertLangFixture(t, db, "app.rb", "ruby", "", "")

	langs := indexedLanguages(db)
	langSet := map[string]bool{}
	for _, l := range langs {
		langSet[l] = true
	}
	if !langSet["go"] {
		t.Error("expected 'go' in languages")
	}
	if !langSet["python"] {
		t.Error("expected 'python' in languages")
	}
	// Error files and files with empty language should not appear.
	if langSet[""] {
		t.Error("empty language should not be in languages")
	}
	// The whole point of the coverage work: a recognised-but-unextractable
	// language is named on its rows so it can be REPORTED as a gap, and must
	// never be presented as a language whose symbols are searchable.
	if langSet["ruby"] {
		t.Error("ruby has no extractor and contributed no symbols; it must not be listed as an indexed language")
	}
	// No duplicates.
	seen := map[string]int{}
	for _, l := range langs {
		seen[l]++
	}
	for lang, count := range seen {
		if count > 1 {
			t.Errorf("language %q appears %d times, want 1", lang, count)
		}
	}
}

// insertLangFixture writes one topology_files row directly. A helper rather than
// four near-identical Exec lines because the (language, content_hash) PAIR is
// what the census reads, and spelling it out per call keeps each fixture's
// intent — parsed, errored, or uncovered — visible at the call site.
func insertLangFixture(t *testing.T, db *sql.DB, path, lang, hash, errMsg string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO topology_files(path, language, mtime_ns, content_hash, error_msg) VALUES (?,?,0,?,?)`,
		path, lang, hash, errMsg); err != nil {
		t.Fatalf("insert fixture %q: %v", path, err)
	}
}

// TestReport_SeparatesUncoveredFromIndexed is the regression for the confident
// empty answer: before the coverage work every one of these rows counted as an
// indexed file, so a Rails app reported full coverage with an empty Map.
func TestReport_SeparatesUncoveredFromIndexed(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "census.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	insertLangFixture(t, db, "main.go", "go", "h1", "")
	insertLangFixture(t, db, "app.rb", "ruby", "", "")
	insertLangFixture(t, db, "lib.rb", "ruby", "", "")
	insertLangFixture(t, db, "data.xml", "xml", "", "")
	insertLangFixture(t, db, "logo.png", "", "", "")
	insertLangFixture(t, db, "broken.go", "", "", "parse failed")

	s := Report(db, dir, nil)
	if s.IndexedFiles != 1 {
		t.Errorf("IndexedFiles = %d, want 1 (only main.go was parsed)", s.IndexedFiles)
	}
	if s.SkippedFiles != 1 {
		t.Errorf("SkippedFiles = %d, want 1", s.SkippedFiles)
	}
	if s.UnrecognisedFiles != 1 {
		t.Errorf("UnrecognisedFiles = %d, want 1 (logo.png)", s.UnrecognisedFiles)
	}
	if got := s.UncoveredFiles["ruby"]; got != 2 {
		t.Errorf("UncoveredFiles[ruby] = %d, want 2", got)
	}
	if got := s.UncoveredFiles["xml"]; got != 1 {
		t.Errorf("UncoveredFiles[xml] = %d, want 1", got)
	}
	if _, ok := s.UncoveredFiles["go"]; ok {
		t.Error("go is extracted; it must not appear in the uncovered census")
	}
}

// TestFormatUncovered_BusiestFirst pins the ordering rather than merely the
// content: the line exists to tell an agent (and us) which extractor to write
// next, which only works if the biggest gap leads. Map iteration would otherwise
// reorder it on every call.
func TestFormatUncovered_BusiestFirst(t *testing.T) {
	got := formatUncovered(map[string]int{"xml": 330, "ruby": 683, "lua": 12, "css": 12})
	want := "ruby (683), xml (330), css (12), lua (12)"
	if got != want {
		t.Errorf("formatUncovered = %q, want %q", got, want)
	}
}

func TestFormatStatus_ReportsCoverageGap(t *testing.T) {
	out := FormatStatus(Status{
		IndexerState:      "idle",
		IndexedFiles:      10,
		UncoveredFiles:    map[string]int{"ruby": 683},
		UnrecognisedFiles: 7,
	}, "/ws")
	for _, want := range []string{"not covered", "ruby (683)", "unrecognised", "7"} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatStatus output missing %q:\n%s", want, out)
		}
	}
}

// A fully-covered workspace must not grow noise: with no gap, neither line is
// rendered at all.
func TestFormatStatus_OmitsCoverageLinesWhenClean(t *testing.T) {
	out := FormatStatus(Status{IndexerState: "idle", IndexedFiles: 10}, "/ws")
	for _, unwanted := range []string{"not covered", "unrecognised"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("FormatStatus should omit %q when there is no gap:\n%s", unwanted, out)
		}
	}
}

func TestFormatStatus_ContainsKeyFields(t *testing.T) {
	s := Status{
		IndexerState: "idle",
		IndexedFiles: 42,
		TotalNodes:   100,
		TotalEdges:   50,
		DBSizeBytes:  1024,
		LastSync:     time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		Languages:    []string{"go", "python"},
		LastError:    "",
	}
	out := FormatStatus(s, "/my/workspace")
	for _, want := range []string{
		"idle",
		"42",
		"100",
		"50",
		"1.0 KiB",
		"/my/workspace",
		"go",
		"python",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatStatus output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatStatus_ShowsLastError(t *testing.T) {
	s := Status{IndexerState: "error", LastError: "disk full"}
	out := FormatStatus(s, "/ws")
	if !strings.Contains(out, "disk full") {
		t.Errorf("FormatStatus should include LastError; got:\n%s", out)
	}
}

// Byte formatting itself is covered by internal/textfmt; what matters here is
// that FormatStatus actually routes the DB size through it.
func TestFormatStatus_DBSizeUsesBinaryUnits(t *testing.T) {
	out := FormatStatus(Status{DBSizeBytes: 2048}, "/ws")
	if !strings.Contains(out, "2.0 KiB") {
		t.Errorf("FormatStatus should render db size as 2.0 KiB; got:\n%s", out)
	}
}

// insertErroredFile writes one skipped-file row with a caller-chosen mtime, so
// the ordering contract of skippedFileErrors is exercisable.
func insertErroredFile(t *testing.T, db *sql.DB, path, errMsg string, mtimeNs int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO topology_files(path, mtime_ns, error_msg) VALUES (?,?,?)`,
		path, mtimeNs, errMsg); err != nil {
		t.Fatalf("insert errored file %q: %v", path, err)
	}
}

// TestReport_SurfacesFileErrors pins the read path that turns error_msg from a
// write-only column into a diagnosis: a skipped file must say WHY, most
// recently touched first.
func TestReport_SurfacesFileErrors(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "errors.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	insertTestFile(t, db, "ok.go")
	insertErroredFile(t, db, "old.go", "parse stopped early: timeout", 100)
	insertErroredFile(t, db, "new.go", "extractor panic: boom", 300)
	insertErroredFile(t, db, "mid.go", "read: permission denied", 200)

	s := Report(db, dir, nil)
	if s.SkippedFiles != 3 {
		t.Fatalf("SkippedFiles = %d, want 3", s.SkippedFiles)
	}
	if len(s.FileErrors) != 3 {
		t.Fatalf("len(FileErrors) = %d, want 3", len(s.FileErrors))
	}
	wantOrder := []string{"new.go", "mid.go", "old.go"}
	for i, want := range wantOrder {
		if s.FileErrors[i].Path != want {
			t.Errorf("FileErrors[%d].Path = %q, want %q (most recently touched first)", i, s.FileErrors[i].Path, want)
		}
	}
	if s.FileErrors[0].Message != "extractor panic: boom" {
		t.Errorf("FileErrors[0].Message = %q, want the recorded reason", s.FileErrors[0].Message)
	}
}

// TestReport_FileErrorsCapped pins the bound: a pathological workspace with
// hundreds of failures must not make Report (which runs on the TUI's poll)
// carry hundreds of rows.
func TestReport_FileErrorsCapped(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "capped.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	for i := range maxStatusFileErrors + 5 {
		insertErroredFile(t, db, fmt.Sprintf("f%03d.go", i), "boom", int64(i))
	}

	s := Report(db, dir, nil)
	if s.SkippedFiles != maxStatusFileErrors+5 {
		t.Fatalf("SkippedFiles = %d, want %d", s.SkippedFiles, maxStatusFileErrors+5)
	}
	if len(s.FileErrors) != maxStatusFileErrors {
		t.Errorf("len(FileErrors) = %d, want the %d-row cap", len(s.FileErrors), maxStatusFileErrors)
	}
}

func TestFormatStatus_ListsSkippedReasons(t *testing.T) {
	out := FormatStatus(Status{
		IndexerState: "idle",
		IndexedFiles: 10,
		SkippedFiles: 25,
		FileErrors: []FileError{
			{Path: "a.go", Message: "parse stopped early: timeout"},
			{Path: "b.py", Message: "extractor panic: boom"},
		},
	}, "/ws")
	for _, want := range []string{
		"skipped files: 25",
		"a.go: parse stopped early: timeout",
		"b.py: extractor panic: boom",
		"(and 23 more)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatStatus output missing %q:\n%s", want, out)
		}
	}
}

// A clean workspace must not grow noise: no skipped files, no reason lines,
// no truncation note.
func TestFormatStatus_OmitsSkippedReasonsWhenClean(t *testing.T) {
	out := FormatStatus(Status{IndexerState: "idle", IndexedFiles: 10}, "/ws")
	if strings.Contains(out, "(and ") {
		t.Errorf("FormatStatus should omit the truncation note when nothing is skipped:\n%s", out)
	}
}

// The truncation note only makes sense next to a non-empty sample: skipped
// files whose reasons were never recorded (or failed to scan) must not print
// "(and N more)" as though a sample had been truncated.
func TestFormatStatus_OmitsTruncationNoteWithoutSample(t *testing.T) {
	out := FormatStatus(Status{IndexerState: "idle", IndexedFiles: 10, SkippedFiles: 3}, "/ws")
	if strings.Contains(out, "(and ") {
		t.Errorf("FormatStatus should omit the truncation note when no reasons were sampled:\n%s", out)
	}
	if !strings.Contains(out, "skipped files: 3") {
		t.Errorf("FormatStatus should still report the skipped count:\n%s", out)
	}
}
