package stats

import (
	"strconv"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// failureFixture opens a fresh database and records a mixed workload: two
// dirty-file refusals from one client build, one from another, a boundary
// refusal, a successful call, and a failure carrying no classification.
func failureFixture(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)

	at := time.UnixMilli(1_000_000)
	record := func(c Call) {
		t.Helper()
		c.Workspace = "/w"
		c.CalledAt = at
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	dirty := Call{
		SessionID: "s1", Tool: "edit_file", ClientName: "claude-code", ClientVersion: "1.0",
		ErrorKind: toolerror.KindDirtyFile, RemediationClass: toolerror.ClassPassDirtyOk, ErrorRetryable: true,
	}
	record(dirty)
	record(dirty)
	older := dirty
	older.ClientVersion = "0.9"
	record(older)
	record(Call{
		SessionID: "s1", Tool: "read_file", ClientName: "claude-code", ClientVersion: "1.0",
		ErrorKind: toolerror.KindWorkspaceBoundary, RemediationClass: toolerror.ClassRepinWorkspace, ErrorRetryable: true,
	})
	record(Call{SessionID: "s1", Tool: "read_file", ClientName: "claude-code", ClientVersion: "1.0", Success: true})
	// No classification: the shape of a pre-v14 row, and of a live failure plumb
	// could not classify. Both must survive to the report as "unclassified".
	record(Call{SessionID: "s1", Tool: "git", ClientName: "claude-code", ClientVersion: "1.0", ErrorMsg: "boom"})
	return db
}

func findBucket(t *testing.T, got []FailureCount, label, tool, version string) FailureCount {
	t.Helper()
	for _, f := range got {
		if f.Label() == label && f.Tool == tool && f.ClientVersion == version {
			return f
		}
	}
	t.Fatalf("no bucket for (%s, %s, %s) in %+v", label, tool, version, got)
	return FailureCount{}
}

// TestFailureSummaryGroupsByKindToolAndClient pins the four grouping keys: two
// identical refusals collapse into one bucket, and a different client build or
// a different tool splits them.
func TestFailureSummaryGroupsByKindToolAndClient(t *testing.T) {
	db := failureFixture(t)

	report, err := db.FailureSummary(0, Filter{})
	got := report.Buckets
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d buckets, want 4 (dirty_file×2 client builds, workspace_boundary, unclassified): %+v", len(got), got)
	}
	// The busiest bucket leads, which is what makes the view usable for triage.
	if got[0].Calls != 2 || got[0].Kind != toolerror.KindDirtyFile {
		t.Errorf("first bucket = %+v, want the 2-call dirty_file group", got[0])
	}

	current := findBucket(t, got, "dirty_file", "edit_file", "1.0")
	if current.Calls != 2 || current.Retryable != 2 {
		t.Errorf("dirty_file @1.0 = %d calls / %d retryable, want 2/2", current.Calls, current.Retryable)
	}
	older := findBucket(t, got, "dirty_file", "edit_file", "0.9")
	if older.Calls != 1 {
		t.Errorf("a different client version did not split the bucket: %+v", older)
	}
	if b := findBucket(t, got, "workspace_boundary", "read_file", "1.0"); b.Calls != 1 {
		t.Errorf("workspace_boundary bucket = %+v, want 1 call", b)
	}
}

// TestFailureSummaryReportsUnclassifiedSeparately is the load-bearing one: a
// failure plumb never classified must appear under its own explicit label,
// neither dropped nor folded into internal — the bucket a reader consults to
// decide whether plumb itself is at fault.
func TestFailureSummaryReportsUnclassifiedSeparately(t *testing.T) {
	db := failureFixture(t)

	report, err := db.FailureSummary(0, Filter{})
	got := report.Buckets
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	b := findBucket(t, got, UnclassifiedLabel, "git", "1.0")
	if b.Calls != 1 {
		t.Errorf("unclassified bucket = %+v, want 1 call", b)
	}
	if b.Kind != "" {
		t.Errorf("unclassified bucket carries Kind %q; the label is a rendering, not a stored value", b.Kind)
	}
	for _, f := range got {
		if f.Kind == toolerror.KindInternal {
			t.Error("an unclassified failure was folded into the internal bucket")
		}
	}
}

// TestFailureSummaryExcludesSuccessesAndHonoursFilter proves it reuses the
// shared Filter predicate rather than a parallel one, and never counts a
// successful call.
func TestFailureSummaryExcludesSuccessesAndHonoursFilter(t *testing.T) {
	db := failureFixture(t)

	var total int64
	allReport, err := db.FailureSummary(0, Filter{})
	all := allReport.Buckets
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	for _, f := range all {
		total += f.Calls
	}
	if total != 5 {
		t.Errorf("counted %d failures, want 5 (the successful call must not appear)", total)
	}

	scopedReport, err := db.FailureSummary(0, Filter{Tool: "edit_file"})
	scoped := scopedReport.Buckets
	if err != nil {
		t.Fatalf("FailureSummary(Tool): %v", err)
	}
	for _, f := range scoped {
		if f.Tool != "edit_file" {
			t.Errorf("Filter.Tool leaked %q into the result", f.Tool)
		}
	}
	if len(scoped) != 2 {
		t.Errorf("got %d buckets for edit_file, want 2 (one per client build): %+v", len(scoped), scoped)
	}

	emptyReport, err := db.FailureSummary(0, Filter{Workspace: "/elsewhere"})
	empty := emptyReport.Buckets
	if err != nil {
		t.Fatalf("FailureSummary(Workspace): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("Filter.Workspace ignored: %+v", empty)
	}
}

// TestFailureSummaryPutsClassifiedFirst reproduces the shape every existing
// installation has after the upgrade: a long tail of pre-v14 failures and a
// handful of newly classified ones. `tool_calls` is never pruned, so ordering
// purely by count would bury every actionable bucket — permanently — under a row
// whose only message is "this predates the feature".
func TestFailureSummaryPutsClassifiedFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	at := time.UnixMilli(1_000_000)
	for i := range 120 {
		tool := []string{"edit_file", "git", "write_file"}[i%3]
		if err := db.Record(Call{
			SessionID: "legacy", Workspace: "/w", Tool: tool, CalledAt: at,
			ClientName: "claude-code", ClientVersion: "1.0", ErrorMsg: "some old prose",
		}); err != nil {
			t.Fatalf("Record legacy: %v", err)
		}
	}
	for range 3 {
		if err := db.Record(Call{
			SessionID: "now", Workspace: "/w", Tool: "edit_file", CalledAt: at,
			ClientName: "claude-code", ClientVersion: "1.0",
			ErrorKind: toolerror.KindDirtyFile, RemediationClass: toolerror.ClassPassDirtyOk, ErrorRetryable: true,
		}); err != nil {
			t.Fatalf("Record classified: %v", err)
		}
	}

	report, err := db.FailureSummary(0, Filter{})
	got := report.Buckets
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no buckets")
	}
	if got[0].Kind != toolerror.KindDirtyFile {
		t.Errorf("first bucket is %q with %d calls; the 3-call classified bucket must lead the "+
			"40-call unclassified ones", got[0].Label(), got[0].Calls)
	}
	if last := got[len(got)-1]; last.Kind != "" {
		t.Errorf("last bucket is %q; the unclassified bucket belongs last", last.Label())
	}
}

// TestFailureSummaryBoundsTheResultSet pins the LIMIT. client_version is copied
// verbatim from the MCP handshake with no cap, so the bucket count is
// caller-influenced and the query must bound it rather than trust the data.
func TestFailureSummaryBoundsTheResultSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for i := range defaultFailureBuckets + 50 {
		if err := db.Record(Call{
			SessionID: "s", Workspace: "/w", Tool: "edit_file", CalledAt: time.UnixMilli(1_000_000),
			ClientName: "claude-code", ClientVersion: strconv.Itoa(i),
			ErrorKind: toolerror.KindDirtyFile, RemediationClass: toolerror.ClassPassDirtyOk,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	report, err := db.FailureSummary(0, Filter{})
	got := report.Buckets
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	if len(got) != defaultFailureBuckets {
		t.Errorf("got %d buckets with no explicit limit, want the %d default", len(got), defaultFailureBuckets)
	}
	report, err = db.FailureSummary(5, Filter{})
	got = report.Buckets
	if err != nil {
		t.Fatalf("FailureSummary(5): %v", err)
	}
	if len(got) != 5 {
		t.Errorf("got %d buckets for limit 5", len(got))
	}
}

// TestFailureSummaryHonoursSince proves the --since window reaches the query
// through the shared Filter, which is what makes the CLI flag able to exclude
// the pre-classification tail.
func TestFailureSummaryHonoursSince(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	if err := db.Record(Call{SessionID: "old", Workspace: "/w", Tool: "git", CalledAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatalf("Record old: %v", err)
	}
	if err := db.Record(Call{
		SessionID: "new", Workspace: "/w", Tool: "edit_file", CalledAt: now,
		ErrorKind: toolerror.KindDirtyFile, RemediationClass: toolerror.ClassPassDirtyOk,
	}); err != nil {
		t.Fatalf("Record new: %v", err)
	}

	report, err := db.FailureSummary(0, Filter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	if len(report.Buckets) != 1 || report.Buckets[0].Kind != toolerror.KindDirtyFile {
		t.Fatalf("Since did not exclude the 48h-old unclassified failure: %+v", report.Buckets)
	}
	if report.UnclassifiedCalls != 0 || report.TotalCalls != 1 {
		t.Errorf("totals ignored the filter: %d unclassified of %d calls", report.UnclassifiedCalls, report.TotalCalls)
	}
}

// TestPreventedIncidentsCountsOnlyGuardKinds proves the PLAN-367 banner count
// sums exactly the write-guard classifications (dirty-file here, via the
// shared failureFixture), and excludes both an unrelated classified failure
// (workspace boundary) and the unclassified row — a loose sum over "all
// failures" would silently inflate this into a number nothing guarded.
func TestPreventedIncidentsCountsOnlyGuardKinds(t *testing.T) {
	db := failureFixture(t)
	got := db.PreventedIncidents(Filter{Workspace: "/w"})
	if got != 3 {
		t.Errorf("PreventedIncidents = %d, want 3 (the three dirty-file rows only)", got)
	}
}

// TestPreventedIncidentsNilDBIsZero mirrors the other stats readers' nil-safe
// contract (SharedReadOnly can return a nil *DB before first use).
func TestPreventedIncidentsNilDBIsZero(t *testing.T) {
	var db *DB
	if got := db.PreventedIncidents(Filter{}); got != 0 {
		t.Errorf("nil DB PreventedIncidents = %d, want 0", got)
	}
}
