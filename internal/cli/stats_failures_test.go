package cli

import (
	"bytes"
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
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, 0, "/w"); err != nil {
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
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, 0, "/w"); err != nil {
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
	if err := printStatsFailures(&out, db, stats.Filter{Workspace: "/w"}, 0, "/w"); err != nil {
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

func TestSinceSuffix_NamesTheWindow(t *testing.T) {
	if got := sinceSuffix(stats.Filter{}); got != "" {
		t.Errorf("an unscoped view claimed a window: %q", got)
	}
	got := sinceSuffix(stats.Filter{Since: time.Now().Add(-7 * 24 * time.Hour)})
	if got != " (last 7d)" {
		t.Errorf("sinceSuffix = %q, want %q", got, " (last 7d)")
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
