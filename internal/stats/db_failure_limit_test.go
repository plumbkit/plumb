package stats

import (
	"strconv"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// seedFailures records n failed calls with the given kind, one per distinct
// client version so each lands in its own bucket.
func seedFailures(t *testing.T, db *DB, tool string, kind toolerror.Kind, n int) {
	t.Helper()
	for i := range n {
		if err := db.Record(Call{
			SessionID: "s", Workspace: "/w", Tool: tool, CalledAt: time.UnixMilli(1_000_000),
			ClientName: "claude-code", ClientVersion: strconv.Itoa(i),
			ErrorKind: kind, ErrorMsg: "boom",
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
}

func openFailuresDB(t *testing.T) *DB {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// TestLimitCannotDeleteTheUnclassifiedBucket is the regression guard for the
// interaction between the two fixes: classified-first ordering makes classified
// buckets consume the limit, so a single LIMIT over the combined ordering would
// cut the unclassified bucket FIRST — quietly undoing the promise that it is
// reported, never dropped.
func TestLimitCannotDeleteTheUnclassifiedBucket(t *testing.T) {
	db := openFailuresDB(t)
	seedFailures(t, db, "edit_file", toolerror.KindDirtyFile, 20)
	seedFailures(t, db, "git", "", 500)

	report, err := db.FailureSummary(3, Filter{})
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	var unclassified int
	for _, f := range report.Buckets {
		if f.Kind == "" {
			unclassified++
		}
	}
	if unclassified == 0 {
		t.Fatalf("limit 3 deleted every unclassified bucket: %+v", report.Buckets)
	}
	if report.UnclassifiedCalls != 500 {
		t.Errorf("UnclassifiedCalls = %d, want 500 — the total must come from the whole "+
			"filter, not from the buckets the limit left", report.UnclassifiedCalls)
	}
	if report.TotalCalls != 520 {
		t.Errorf("TotalCalls = %d, want 520", report.TotalCalls)
	}
	if !report.Incomplete() {
		t.Error("a view showing 3 of 20 classified buckets did not report itself as incomplete")
	}
	if report.ShownCalls() >= report.TotalCalls {
		t.Errorf("ShownCalls %d >= TotalCalls %d; the footer would claim the view is complete",
			report.ShownCalls(), report.TotalCalls)
	}
}

// TestUnclassifiedBucketsAreThemselvesBounded proves the pinned side did not
// become an unbounded escape hatch: it groups by tool and client too, so it
// carries its own cap — and the honest count survives that cap.
func TestUnclassifiedBucketsAreThemselvesBounded(t *testing.T) {
	db := openFailuresDB(t)
	seedFailures(t, db, "git", "", unclassifiedBucketCap+40)

	report, err := db.FailureSummary(0, Filter{})
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	if len(report.Buckets) != unclassifiedBucketCap {
		t.Errorf("got %d unclassified buckets, want the %d cap", len(report.Buckets), unclassifiedBucketCap)
	}
	if want := int64(unclassifiedBucketCap + 40); report.UnclassifiedCalls != want {
		t.Errorf("UnclassifiedCalls = %d, want %d even though the buckets were capped",
			report.UnclassifiedCalls, want)
	}
}

// TestReportIsNotTruncatedWhenEverythingFits keeps the footer honest in the
// other direction — a complete view must not claim to be partial.
func TestReportIsNotTruncatedWhenEverythingFits(t *testing.T) {
	db := openFailuresDB(t)
	seedFailures(t, db, "edit_file", toolerror.KindDirtyFile, 3)

	report, err := db.FailureSummary(0, Filter{})
	if err != nil {
		t.Fatalf("FailureSummary: %v", err)
	}
	if report.Incomplete() {
		t.Errorf("a complete view reported itself incomplete: %d of %d buckets",
			len(report.Buckets), report.TotalBuckets)
	}
	if report.ShownCalls() != report.TotalCalls {
		t.Errorf("ShownCalls %d != TotalCalls %d on a complete view", report.ShownCalls(), report.TotalCalls)
	}
}
