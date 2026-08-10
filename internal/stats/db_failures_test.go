package stats

import (
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

	got, err := db.FailureSummary(Filter{})
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

	got, err := db.FailureSummary(Filter{})
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
	all, err := db.FailureSummary(Filter{})
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	for _, f := range all {
		total += f.Calls
	}
	if total != 5 {
		t.Errorf("counted %d failures, want 5 (the successful call must not appear)", total)
	}

	scoped, err := db.FailureSummary(Filter{Tool: "edit_file"})
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

	empty, err := db.FailureSummary(Filter{Workspace: "/elsewhere"})
	if err != nil {
		t.Fatalf("FailureSummary(Workspace): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("Filter.Workspace ignored: %+v", empty)
	}
}
