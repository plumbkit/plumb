package cli

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/toolerror"
)

func failuresDB(t *testing.T, calls ...stats.Call) *stats.DB {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("Open DB: %v", err)
	}
	t.Cleanup(db.Close)
	for _, c := range calls {
		c.Workspace = "/w"
		c.SessionID = "s1"
		c.CalledAt = time.Now()
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	return db
}

func TestPrintStatsFailures_RendersEachBucket(t *testing.T) {
	db := failuresDB(t,
		stats.Call{
			Tool: "edit_file", ClientName: "claude-code", ClientVersion: "1.0",
			ErrorKind: toolerror.KindDirtyFile, RemediationClass: toolerror.ClassPassDirtyOk, ErrorRetryable: true,
		},
		stats.Call{
			Tool: "git", ClientName: "claude-code", ClientVersion: "1.0",
			ErrorKind: toolerror.KindGitPolicy, RemediationClass: toolerror.ClassEnablePolicy,
		},
		stats.Call{Tool: "read_file", ClientName: "claude-code", ClientVersion: "1.0", Success: true},
	)

	var out bytes.Buffer
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, statsView{workspace: "/w"}); err != nil {
		t.Fatalf("printStatsFailures: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Failures by Kind", "dirty_file", "edit_file", "git_policy", "claude-code 1.0", "Retryable"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "read_file") {
		t.Errorf("a successful call reached the failure view:\n%s", got)
	}
	// Assert on wording common to BOTH the singular and plural branches: guarding
	// only the plural would let a regression that fires the note for exactly one
	// row slip through saying "1 failure carries no classification".
	if strings.Contains(got, "no classification") {
		t.Errorf("the unclassified note fired with no unclassified rows:\n%s", got)
	}
}

// TestPrintStatsFailures_LabelsUnclassified proves an unclassified failure is
// shown under its own label with a note explaining it, rather than dropped or
// silently presented as a kind plumb chose.
func TestPrintStatsFailures_LabelsUnclassified(t *testing.T) {
	db := failuresDB(t, stats.Call{Tool: "git", ClientName: "claude-code", ErrorMsg: "boom"})

	var out bytes.Buffer
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, statsView{workspace: "/w"}); err != nil {
		t.Fatalf("printStatsFailures: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, stats.UnclassifiedLabel) {
		t.Errorf("unclassified bucket missing from the table:\n%s", got)
	}
	if !strings.Contains(got, "1 failure carries no classification") {
		t.Errorf("expected the singular unclassified note:\n%s", got)
	}
	if strings.Contains(got, "internal") {
		t.Errorf("an unclassified failure was rendered as the internal kind:\n%s", got)
	}
}

func TestPrintStatsFailures_NoFailuresSaysSo(t *testing.T) {
	db := failuresDB(t, stats.Call{Tool: "read_file", Success: true})

	var out bytes.Buffer
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, statsView{workspace: "/w"}); err != nil {
		t.Fatalf("printStatsFailures: %v", err)
	}
	if !strings.Contains(out.String(), "No failures recorded") {
		t.Errorf("empty failure view did not say so:\n%s", out.String())
	}
}

func TestStatsClientCell(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		in         stats.FailureCount
	}{
		{"anonymous client", "—", stats.FailureCount{}},
		{"no version", "codex", stats.FailureCount{ClientName: "codex"}},
		{"full build", "codex 2.1", stats.FailureCount{ClientName: "codex", ClientVersion: "2.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statsClientCell(tc.in); got != tc.want {
				t.Errorf("statsClientCell = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrintStatsFailures_UnknownRetryabilityIsNotZero pins the distinction the
// column must preserve: 0 means "checked, none were retryable"; the unclassified
// bucket has no answer at all, and printing 0 there would be a claim about rows
// plumb never classified.
func TestPrintStatsFailures_UnknownRetryabilityIsNotZero(t *testing.T) {
	unclassified := stats.FailureCount{Tool: "git", Calls: 4}
	if got := retryableCell(unclassified); got != "—" {
		t.Errorf("retryableCell(unclassified) = %q, want an em dash", got)
	}
	checked := stats.FailureCount{Kind: toolerror.KindGitPolicy, Tool: "git", Calls: 4}
	if got := retryableCell(checked); got != "0" {
		t.Errorf("retryableCell(classified, none retryable) = %q, want %q", got, "0")
	}
}

// TestSinceSuffix_EchoesTheRequest pins that the window label comes from what
// the user asked for, not from wall-clock at render time. The filter's Since is
// deliberately set NOWHERE NEAR the requested window: a renderer that measured
// time.Since(filter.Since) would report ~30d here, and would in real use drift
// by however long the queries took — three full-table aggregates run between
// building the filter and printing the label.
func TestSinceSuffix_EchoesTheRequest(t *testing.T) {
	if got := (statsView{}).sinceSuffix(); got != "" {
		t.Errorf("an unscoped view claimed a window: %q", got)
	}
	for _, want := range []string{"7d", "2w", "1h", "90m", "45s"} {
		t.Run(want, func(t *testing.T) {
			v := statsView{since: want}
			if got := v.sinceSuffix(); got != " (last "+want+")" {
				t.Errorf("sinceSuffix() = %q, want %q", got, " (last "+want+")")
			}
		})
	}
}

func TestParseAge(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "90m", want: 90 * time.Minute},
		{in: "24h", want: 24 * time.Hour},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "2w", want: 14 * 24 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "", bad: true},
		{in: "yesterday", bad: true},
		{in: "0d", bad: true},
		{in: "-3h", bad: true},
		{in: "1.5d", bad: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseAge(tc.in)
			if tc.bad {
				if err == nil {
					t.Errorf("parseAge(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAge(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseAge(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestPrintStatsFailures_TruncationIsAnnounced covers the reader-facing half of
// the bound: a view showing three buckets of five hundred looks exactly like a
// view showing three of three unless it says otherwise.
func TestPrintStatsFailures_TruncationIsAnnounced(t *testing.T) {
	calls := make([]stats.Call, 0, 20)
	for i := range 20 {
		calls = append(calls, stats.Call{
			Tool: "edit_file", ClientName: "claude-code", ClientVersion: strconv.Itoa(i),
			ErrorKind: toolerror.KindDirtyFile, RemediationClass: toolerror.ClassPassDirtyOk, ErrorRetryable: true,
		})
	}
	db := failuresDB(t, calls...)

	var out bytes.Buffer
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, statsView{workspace: "/w", limit: 3}); err != nil {
		t.Fatalf("printStatsFailures: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "showing 3 of 20 buckets") {
		t.Errorf("a bounded view did not say it was bounded:\n%s", got)
	}
	if !strings.Contains(got, "3 of 20 failed calls") {
		t.Errorf("the footer did not account for the calls the view omits:\n%s", got)
	}

	// The same data with room for every bucket must not claim to be partial.
	out.Reset()
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, statsView{workspace: "/w"}); err != nil {
		t.Fatalf("printStatsFailures: %v", err)
	}
	if strings.Contains(out.String(), "showing") {
		t.Errorf("a complete view claimed to be truncated:\n%s", out.String())
	}
}

// TestPrintStatsFailures_UnclassifiedNoteCountsBeyondTheLimit is the CLI half of
// the same guarantee: the note must report every unclassified failure, not just
// the ones whose buckets survived the cap.
func TestPrintStatsFailures_UnclassifiedNoteCountsBeyondTheLimit(t *testing.T) {
	calls := make([]stats.Call, 0, 40)
	for i := range 40 {
		calls = append(calls, stats.Call{
			Tool: "git", ClientName: "claude-code", ClientVersion: strconv.Itoa(i), ErrorMsg: "old prose",
		})
	}
	db := failuresDB(t, calls...)

	var out bytes.Buffer
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, statsView{workspace: "/w", limit: 2}); err != nil {
		t.Fatalf("printStatsFailures: %v", err)
	}
	if !strings.Contains(out.String(), "40 failures carry no classification") {
		t.Errorf("the note counted only the rendered buckets:\n%s", out.String())
	}
}

// TestPrintStatsFailures_EmptyStateNamesTheWindow guards the second empty state:
// "every call succeeded" reads as "no failures ever" unless the window is named.
func TestPrintStatsFailures_EmptyStateNamesTheWindow(t *testing.T) {
	db := failuresDB(t, stats.Call{Tool: "read_file", Success: true})

	var out bytes.Buffer
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, statsView{workspace: "/w", since: "30m"}); err != nil {
		t.Fatalf("printStatsFailures: %v", err)
	}
	if !strings.Contains(out.String(), "(last 30m)") {
		t.Errorf("the empty state did not name the window it was scoped to:\n%s", out.String())
	}
}

// TestParseAgeRejectsOverflow guards the multiply. A Duration is an int64 of
// nanoseconds, so a large day count wraps NEGATIVE, which would put the window's
// start in the future and report an empty database on a full one — a confident
// wrong answer, worse than the error the input deserves.
func TestParseAgeRejectsOverflow(t *testing.T) {
	for _, in := range []string{"9223372036854775807d", "100000000000d", "9223372036854775807w", "20000000000w"} {
		t.Run(in, func(t *testing.T) {
			got, err := parseAge(in)
			if err == nil {
				t.Errorf("parseAge(%q) = %v, want an error", in, got)
			}
			if got < 0 {
				t.Errorf("parseAge(%q) returned a NEGATIVE age (%v): the window would start in the future", in, got)
			}
		})
	}
	// The largest value that still fits must keep working, so the guard is a
	// bound rather than a blanket refusal of large ages.
	if _, err := parseAge("100000d"); err != nil {
		t.Errorf("parseAge(100000d): %v", err)
	}
}
