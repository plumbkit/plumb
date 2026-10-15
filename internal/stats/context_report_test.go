package stats

import (
	"encoding/json"
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
		if errs != "" {
			errs += ","
		}
		errs += `{"file":"a.go","line":9,"severity":"warning","message":"a warning is not an error"}`
		return "applied 1 edit\n[diagnostics: authoritative post-write pass]\ndiagnostics delta: {\"fresh\":true,\"scopes\":{\"edited_file\":\"fresh\",\"cross_file\":\"incomplete\"},\"new_errors\":[" + errs + "],\"resolved\":[],\"pre_existing\":0}"
	}
	unconfirmed := "applied 1 edit\ndiagnostics delta: {\"fresh\":false,\"scopes\":{\"edited_file\":\"timeout\",\"cross_file\":\"not_checked\"},\"new_errors\":[{\"file\":\"x.go\",\"line\":1,\"severity\":\"error\",\"message\":\"stale\"}],\"resolved\":[],\"pre_existing\":0}"
	// Stream s1: a prose pack, then the work it led to.
	rec("/w", "s1", "", "context_for_task", `{"task":"the checkout charges twice"}`, contextFixturePack, true, week1)
	rec("/w", "s1", "", "search_in_files", `{"pattern":"ledger"}`, "", true, week1)
	rec("/w", "s1", "", "read_file", `{"file_path":"/w/api/checkout.go"}`, "", true, week1)
	rec("/w", "s1", "", "edit_file", `{"file_path":"api/checkout.go"}`, fresh(1), true, week1)
	rec("/w", "s1", "", "search_in_files", `{"pattern":"after the first edit"}`, "", true, week1)
	rec("/w", "s1", "", "edit_file", `{"file_path":"store/ledger.go"}`, fresh(0), true, week1)
	rec("/w", "s1", "", "edit_file", `{"file_path":"store/ledger.go"}`, "", false, week1)
	rec("/w", "s1", "", "edit_file", `{"file_path":"never/written.go"}`, "", false, week1)
	rec("/w", "s1", "", "undo_edit", `{}`, "", true, week1)
	rec("/w", "s1", "", "run_task", `{"slot":"test"}`, "$ go test\nFAIL\n→ stopped: step 1/1 failed (exit 1)\n", true, week1)
	rec("/w", "s1", "", "run_task", `{"slot":"test"}`, "$ go test\nok\n→ ok\n", true, week1)
	rec("/w", "s1", "", "tool_feedback", `{"tool":"context_for_task","missing":"the ledger's callers"}`, "", true, week1)
	// Stream s2/a1: edits without a pack.
	rec("/w", "s2", "a1", "edit_file", `{"file_path":"x.go"}`, unconfirmed, true, week1)
	rec("/w", "s2", "a1", "edit_file", `{"file_path":"/w/x.go"}`, "", true, week1)
	rec("/w", "s2", "a1", "run_task", `{"slot":"test"}`, "→ ok\n", true, week1)
	// Stream s3: edits without a pack, its last test run red.
	rec("/w", "s3", "", "edit_file", `{"file_path":"y.go"}`, "", true, week1)
	rec("/w", "s3", "", "run_task", `{"slot":"test"}`, "→ ok\n", true, week1)
	rec("/w", "s3", "", "run_task", `{"slot":"test"}`, "→ stopped: step 1/1 failed (exit 1)\n", true, week1)
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
		{"streams", w.Streams, 3},
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
		{"with: edits", w.WithPack.Edits, 4},
		{"with: failed edits", w.WithPack.EditFailures, 2},
		{"with: diagnosed", w.WithPack.DiagnosedEdits, 2},
		{"with: new-error edits", w.WithPack.NewErrorEdits, 1},
		{"with: undos", w.WithPack.Undos, 1},
		{"with: test runs", w.WithPack.TestRuns, 2},
		{"with: test failures", w.WithPack.TestFailures, 1},
		{"with: ended green", w.WithPack.EndedGreen, 1},
		{"with: files", w.WithPack.FilesEdited, 2},
		{"without: streams", w.WithoutPack.Streams, 2},
		{"without: edits", w.WithoutPack.Edits, 3},
		{"without: files (x.go in two spellings, y.go)", w.WithoutPack.FilesEdited, 2},
		{"without: unconfirmed delta is no diagnosis", w.WithoutPack.DiagnosedEdits, 0},
		{"without: tested streams", w.WithoutPack.StreamsWithTests, 2},
		{"without: ended green (s3 ended red)", w.WithoutPack.EndedGreen, 1},
		{"week 2 streams", r.Weeks[1].Streams, 1},
		{"week 2 editing streams", r.Weeks[1].WithoutPack.Streams, 0},
		{"total streams", r.Total.Streams, 4},
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

// A second pack closes the first one's window: what the agent did between them
// is scored against the first pack, and the second starts empty.
func TestContextReport_ANewPackClosesTheWindow(t *testing.T) {
	db := openFailuresDB(t)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	for i, c := range []Call{
		{Tool: "context_for_task", InputJSON: `{"files":["api/checkout.go"]}`, OutputText: contextFixturePack, Success: true},
		{Tool: "edit_file", InputJSON: `{"file_path":"cart/cart.go"}`, Success: true},
		{Tool: "context_for_task", InputJSON: `{"files":["store/ledger.go"]}`, OutputText: "context_for_task root=/r\nseeds (1 resolved, 0 unresolved):\n  file store/ledger.go [go]\n", Success: true},
	} {
		c.Workspace, c.SessionID, c.CalledAt = "/w", "s", at.Add(time.Duration(i)*time.Second)
		if err := db.Record(c); err != nil {
			t.Fatal(err)
		}
	}
	r, err := db.ContextReport(Filter{Workspace: "/w"}, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p := r.Total
	if p.Packs != 2 || p.WindowsWithEdits != 1 || p.EditedFiles != 1 || p.EditedFilesInPack != 1 || p.PackFiles != 3 || p.PackFilesUsed != 1 {
		t.Errorf("packs=%d windows=%d edited=%d inPack=%d packFiles=%d used=%d, want 2 1 1 1 3 1",
			p.Packs, p.WindowsWithEdits, p.EditedFiles, p.EditedFilesInPack, p.PackFiles, p.PackFilesUsed)
	}
}

// recordAll records calls in order, one second apart from at.
func recordAll(t *testing.T, db *DB, at time.Time, calls []Call) {
	t.Helper()
	for i, c := range calls {
		c.CalledAt = at.Add(time.Duration(i) * time.Second)
		if err := db.Record(c); err != nil {
			t.Fatal(err)
		}
	}
}

// A stream is read whole when any of its calls is in the workspace, so a pack in
// one workspace and the edits it led to under another stay one stream; its work
// before the first pack is not counted for the pack; previews are not edits;
// failed packs, reads and notes count for nothing.
func TestContextReport_StreamsAreWholeAndScoredFromTheFirstPack(t *testing.T) {
	db := openFailuresDB(t)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	pack := Call{Workspace: "/w", SessionID: "s", Tool: "context_for_task", InputJSON: `{"files":["api/checkout.go"]}`, OutputText: contextFixturePack, Success: true}
	recordAll(t, db, at, []Call{
		{Workspace: "/other", SessionID: "s", Tool: "edit_file", InputJSON: `{"file_path":"early.go"}`, Success: true},
		{Workspace: "/w", SessionID: "s", Tool: "context_for_task", InputJSON: `{"files":["x.go"]}`, Success: false},
		pack,
		{Workspace: "/other", SessionID: "s", Tool: "read_file", InputJSON: `{"file_path":"api/checkout.go"}`, Success: false},
		{Workspace: "/other", SessionID: "s", Tool: "read_file", InputJSON: `{"file_path":"cart/cart.go"}`, Success: true},
		{Workspace: "/other", SessionID: "s", Tool: "rename_symbol", InputJSON: `{"uri":"cart/cart.go","new_name":"X"}`, Success: true},
		{Workspace: "/other", SessionID: "s", Tool: "find_replace", InputJSON: `{"path":"cart","find":"a","replace":"b"}`, Success: true},
		{Workspace: "/other", SessionID: "s", Tool: "find_replace", InputJSON: `{"path":"cart","find":"a","replace":"b","dry_run":false}`, Success: true},
		{Workspace: "/other", SessionID: "s", Tool: "edit_file", InputJSON: `{"file_path":"api/checkout.go"}`, Success: true},
		{Workspace: "/other", SessionID: "s", Tool: "move_symbol", InputJSON: `{"source_uri":"store/ledger.go","destination_uri":"store/moved.go","name":"Total","dry_run":false}`, Success: true},
		{Workspace: "/other", SessionID: "s", Tool: "rename_file", InputJSON: `{"from":"store/moved.go","to":"store/renamed.go"}`, Success: true},
		{Workspace: "/other", SessionID: "s", Tool: "tool_feedback", InputJSON: `{"tool":"x","note":"lost"}`, Success: false},
	})
	r, err := db.ContextReport(Filter{Workspace: "/w", Since: at.Add(-time.Hour)}, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p := r.Total
	checks := []struct {
		name      string
		got, want int
	}{
		{"packs (the failed one opens nothing)", p.Packs, 1},
		{"with: streams", p.WithPack.Streams, 1},
		{"without: streams (early.go is pre-pack)", p.WithoutPack.Streams, 0},
		{"with: edits (find_replace, edit_file, move_symbol, rename_file; previews and pre-pack work not)", p.WithPack.Edits, 4},
		{"with: files (find_replace names none; move and rename name theirs)", p.WithPack.FilesEdited, 4},
		{"edited files in pack", p.EditedFilesInPack, 1},
		{"pack files used (the failed read is not a use)", p.PackFilesUsed, 2},
		{"seed rereads (only the seed's file, and the failed read is none)", p.SeedRereads, 0},
		{"feedback (a failed note is none)", p.Feedback, 0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// A stream belongs to the week of its first call; a pack, to the week of the
// pack.
func TestContextReport_WeeksByFirstCallAndByPack(t *testing.T) {
	db := openFailuresDB(t)
	mon := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	recordAll(t, db, mon, []Call{{Workspace: "/w", SessionID: "s", Tool: "read_file", InputJSON: `{"file_path":"a.go"}`, Success: true}})
	recordAll(t, db, mon.Add(8*24*time.Hour), []Call{{Workspace: "/w", SessionID: "s", Tool: "context_for_task", InputJSON: `{"files":["a.go"]}`, OutputText: contextFixturePack, Success: true}})
	r, err := db.ContextReport(Filter{Workspace: "/w", Since: mon.Add(-time.Hour)}, mon.Add(20*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Weeks) != 2 || r.Weeks[0].Streams != 1 || r.Weeks[0].Packs != 0 || r.Weeks[1].Streams != 0 || r.Weeks[1].Packs != 1 {
		t.Errorf("weeks = %+v, want the stream in week 1 and its pack in week 2", r.Weeks)
	}
}

// Leads are call input printed back, so a secret in one is redacted, and the
// JSON's lists are never null.
func TestContextReport_LeadsRedactedAndJSONListsNeverNull(t *testing.T) {
	db := openFailuresDB(t)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	secret := "ghp_" + strings.Repeat("A1b2", 9)
	recordAll(t, db, at, []Call{
		{Workspace: "/w", SessionID: "s", Tool: "context_for_task", InputJSON: `{"files":["a.go"]}`, OutputText: contextFixturePack, Success: true},
		{Workspace: "/w", SessionID: "s", Tool: "search_in_files", InputJSON: `{"pattern":"token ` + secret + `"}`, Success: true},
	})
	r, err := db.ContextReport(Filter{Workspace: "/w", Since: at.Add(-time.Hour)}, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Leads.Searches) != 1 || strings.Contains(r.Leads.Searches[0].Text, secret) {
		t.Errorf("search lead %+v carries the secret", r.Leads.Searches)
	}
	b, err := json.Marshal(EmptyContextReport(at, at))
	if err != nil || strings.Contains(string(b), "null") {
		t.Errorf("empty report JSON has a null list: %s (%v)", b, err)
	}
}
