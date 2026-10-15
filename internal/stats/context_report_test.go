package stats

import (
	"strings"
	"testing"
	"time"
)

// contextFixturePack is a pack as context_for_task renders one (the format is
// pinned against the renderer in internal/tools' context_report_formats_test).
const contextFixturePack = `context_for_task root=/r intent=change max_bytes=12000 (pack<=10976, 1024 reserved)
seeds (1 resolved, 0 unresolved) — proposed from the task prose, none were given; pass files or symbols to choose your own:
  symbol Checkout — api/checkout.go:11 function [go] — found by search on: checkout
11	func Checkout() {}
related (1, ranked; e4 language-server edge, e3 extractor edge, e2 derived edge, e1 heuristic edge):
  [e3 extractor] (*Cart).Total — cart/cart.go:45 method; callee of Checkout
gaps:
  - affected tests: 2 test(s) outside within/corpora were left out of the estimate
`

func contextReportFixture(t *testing.T) (*DB, time.Time) {
	t.Helper()
	db := openFailuresDB(t)
	week1 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC) // a Tuesday
	step := 0
	rec := func(ws, sid, agent, tool, in, out string, ok bool, at time.Time) {
		t.Helper()
		step++
		c := Call{
			Workspace: ws, SessionID: sid, LogicalAgent: agent, Tool: tool, InputJSON: in, OutputText: out,
			Success: ok, CalledAt: at.Add(time.Duration(step) * time.Second),
		}
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	fresh := func(n int) string {
		errs := ""
		var errsSb36 strings.Builder
		for i := range n {
			if i > 0 {
				errsSb36.WriteString(",")
			}
			errsSb36.WriteString(`{"file":"a.go","line":1,"severity":"error","message":"x"}`)
		}
		errs += errsSb36.String()
		return "applied 1 edit\n[diagnostics: authoritative post-write pass]\ndiagnostics delta: {\"fresh\":true,\"scopes\":{\"edited_file\":\"fresh\",\"cross_file\":\"incomplete\"},\"new_errors\":[" + errs + "],\"resolved\":[],\"pre_existing\":0}"
	}
	// Stream s1: a prose pack, then the work it led to.
	rec("/w", "s1", "", "context_for_task", `{"task":"the checkout charges twice"}`, contextFixturePack, true, week1)
	rec("/w", "s1", "", "search_in_files", `{"pattern":"ledger"}`, "", true, week1)
	rec("/w", "s1", "", "read_file", `{"file_path":"/w/api/checkout.go"}`, "", true, week1)
	rec("/w", "s1", "", "edit_file", `{"file_path":"api/checkout.go"}`, fresh(1), true, week1)
	rec("/w", "s1", "", "search_in_files", `{"pattern":"after the first edit"}`, "", true, week1)
	rec("/w", "s1", "", "edit_file", `{"file_path":"store/ledger.go"}`, fresh(0), true, week1)
	rec("/w", "s1", "", "edit_file", `{"file_path":"store/ledger.go"}`, "", false, week1)
	rec("/w", "s1", "", "undo_edit", `{}`, "", true, week1)
	rec("/w", "s1", "", "run_task", `{"slot":"test"}`, "$ go test\nFAIL\n→ stopped: step 1/1 failed (exit 1)\n", true, week1)
	rec("/w", "s1", "", "run_task", `{"slot":"test"}`, "$ go test\nok\n→ ok\n", true, week1)
	rec("/w", "s1", "", "tool_feedback", `{"tool":"context_for_task","missing":"the ledger's callers"}`, "", true, week1)
	// Stream s2/a1: edits without a pack.
	rec("/w", "s2", "a1", "edit_file", `{"file_path":"x.go"}`, "", true, week1)
	rec("/w", "s2", "a1", "edit_file", `{"file_path":"/w/x.go"}`, "", true, week1)
	rec("/w", "s2", "a1", "run_task", `{"slot":"test"}`, "→ ok\n", true, week1)
	// Stream s2/a2, a week later: reads only.
	rec("/w", "s2", "a2", "read_file", `{"file_path":"x.go"}`, "", true, week1.Add(7*24*time.Hour))
	// Another workspace: never counted.
	rec("/other", "s9", "", "context_for_task", `{"files":["a.go"]}`, contextFixturePack, true, week1)
	return db, week1
}

func TestContextReport_ScoresPacksAndOutcomes(t *testing.T) {
	db, week1 := contextReportFixture(t)
	r, err := db.ContextReport(Filter{Workspace: "/w", Since: week1.Add(-time.Hour)}, week1.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Weeks) != 2 || !r.Weeks[0].Start.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weeks = %+v, want two, the first starting Monday 2026-10-05", r.Weeks)
	}
	w := r.Weeks[0]
	checks := []struct {
		name      string
		got, want int
	}{
		{"streams", w.Streams, 2},
		{"streams with pack", w.StreamsWithPack, 1},
		{"packs", w.Packs, 1},
		{"prose packs", w.ProsePacks, 1},
		{"proposed seeds", w.ProposedSeeds, 1},
		{"proposed seeds used", w.ProposedSeedsUsed, 1},
		{"windows with edits", w.WindowsWithEdits, 1},
		{"edited files", w.EditedFiles, 2},
		{"edited files in pack", w.EditedFilesInPack, 1},
		{"pack files", w.PackFiles, 2},
		{"pack files used", w.PackFilesUsed, 1},
		{"searches before first edit", w.SearchesBeforeEdit, 1},
		{"seed rereads", w.SeedRereads, 1},
		{"feedback", w.Feedback, 1},
		{"with: streams", w.WithPack.Streams, 1},
		{"with: edits", w.WithPack.Edits, 3},
		{"with: failed edits", w.WithPack.EditFailures, 1},
		{"with: diagnosed", w.WithPack.DiagnosedEdits, 2},
		{"with: new-error edits", w.WithPack.NewErrorEdits, 1},
		{"with: undos", w.WithPack.Undos, 1},
		{"with: test runs", w.WithPack.TestRuns, 2},
		{"with: test failures", w.WithPack.TestFailures, 1},
		{"with: ended green", w.WithPack.EndedGreen, 1},
		{"with: files", w.WithPack.FilesEdited, 2},
		{"without: streams", w.WithoutPack.Streams, 1},
		{"without: edits", w.WithoutPack.Edits, 2},
		{"without: files (one file, two spellings)", w.WithoutPack.FilesEdited, 1},
		{"without: ended green", w.WithoutPack.EndedGreen, 1},
		{"week 2 streams", r.Weeks[1].Streams, 1},
		{"week 2 editing streams", r.Weeks[1].WithoutPack.Streams, 0},
		{"total streams", r.Total.Streams, 3},
		{"total packs (other workspace excluded)", r.Total.Packs, 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if len(r.Leads.MissedFiles) != 1 || r.Leads.MissedFiles[0].Text != "store/ledger.go" {
		t.Errorf("missed files = %+v, want store/ledger.go", r.Leads.MissedFiles)
	}
	if len(r.Leads.Searches) != 1 || r.Leads.Searches[0].Text != "search_in_files: ledger" {
		t.Errorf("searches = %+v, want only the one before the first edit", r.Leads.Searches)
	}
	if len(r.Leads.Gaps) != 1 || r.Leads.Gaps[0].Text != "affected tests: N test(s) outside within/corpora were left out of the estimate" {
		t.Errorf("gaps = %+v", r.Leads.Gaps)
	}
	if len(r.Leads.Feedback) != 1 || r.Leads.Feedback[0].Tool != "context_for_task" || r.Leads.Feedback[0].Missing != "the ledger's callers" {
		t.Errorf("feedback = %+v", r.Leads.Feedback)
	}
}

func TestContextReport_AllWorkspacesAndEmpty(t *testing.T) {
	db, week1 := contextReportFixture(t)
	r, err := db.ContextReport(Filter{Since: week1.Add(-time.Hour)}, week1.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r.Total.Packs != 2 {
		t.Errorf("all workspaces: packs = %d, want 2", r.Total.Packs)
	}
	empty, err := db.ContextReport(Filter{Workspace: "/nowhere"}, week1)
	if err != nil || empty.Total.Streams != 0 || len(empty.Weeks) != 0 {
		t.Errorf("empty workspace: %+v (%v)", empty, err)
	}
	var nilDB *DB
	if _, err := nilDB.ContextReport(Filter{}, week1); err != nil {
		t.Errorf("nil DB: %v", err)
	}
}

func TestSamePath(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"api/x.go", "/w/api/x.go", true},
		{"./api/x.go", "api/x.go", true},
		{"api/x.go", "/w/papi/x.go", false},
		{"x.go", "y.go", false},
	} {
		if got := samePath(c.a, c.b); got != c.want {
			t.Errorf("samePath(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestClampLeadKeepsRunesWhole(t *testing.T) {
	s := ""
	var sSb154 strings.Builder
	for range 200 {
		sSb154.WriteString("é")
	}
	s += sSb154.String()
	got := clampLead(s)
	if len(got) > contextReportLeadBytes+len("…") {
		t.Errorf("clampLead length %d", len(got))
	}
	for _, r := range got {
		if r == '�' {
			t.Fatal("clampLead split a rune")
		}
	}
}
