package stats

// context_report.go — `plumb stats --context`: what context_for_task packs did
// for the agents that asked for them, measured from the calls plumb already
// records, week by week. No new table and no new write: every number is derived
// at read time from tool_calls, so it covers whatever history the database holds.
//
// Three questions, each answered with a proxy whose limits the renderer states:
//
//   - Uptake: how many agent streams (a session, or one logical agent on a
//     shared connection) asked for a pack, and how many of those packs started
//     from task prose.
//   - Pack quality: of the files an agent went on to edit after a pack, how many
//     the pack had named (recall); of the files a pack named, how many the agent
//     went on to read or edit (use); how many searches it still ran before its
//     first edit; whether the seeds discovery proposed were used.
//   - Maintenance quality: for streams that edited, with a pack and without one,
//     how often an edit failed, introduced a compile error the post-write
//     diagnostics caught, or was undone, and how the test runs ended.
//
// Plus leads, the agents' own pointers at what is missing: files edited after a
// pack that the pack did not name, what agents searched for after a pack, the
// gaps packs admitted most often, and the notes agents left with tool_feedback.
//
// Only plumb calls are visible: a client's native reads, searches and edits are
// not recorded, so every rate is over plumb's lane alone.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Report bounds.
const (
	contextReportMaxLeads    = 10
	contextReportMaxFeedback = 20
	contextReportLeadBytes   = 160
)

// ContextOutcome is the edit and test record of a set of agent streams.
type ContextOutcome struct {
	Streams          int `json:"streams"`
	Edits            int `json:"edits"`
	EditFailures     int `json:"edit_failures"`
	DiagnosedEdits   int `json:"diagnosed_edits"`
	NewErrorEdits    int `json:"new_error_edits"`
	Undos            int `json:"undos"`
	TestRuns         int `json:"test_runs"`
	TestFailures     int `json:"test_failures"`
	StreamsWithTests int `json:"streams_with_tests"`
	EndedGreen       int `json:"ended_green"`
	FilesEdited      int `json:"files_edited"`
}

func (o *ContextOutcome) add(b ContextOutcome) {
	o.Streams += b.Streams
	o.Edits += b.Edits
	o.EditFailures += b.EditFailures
	o.DiagnosedEdits += b.DiagnosedEdits
	o.NewErrorEdits += b.NewErrorEdits
	o.Undos += b.Undos
	o.TestRuns += b.TestRuns
	o.TestFailures += b.TestFailures
	o.StreamsWithTests += b.StreamsWithTests
	o.EndedGreen += b.EndedGreen
	o.FilesEdited += b.FilesEdited
}

// ContextPeriod is one week of the report, or its total.
type ContextPeriod struct {
	Start time.Time `json:"start"`

	Streams         int `json:"streams"`
	StreamsWithPack int `json:"streams_with_pack"`
	Packs           int `json:"packs"`
	ProsePacks      int `json:"prose_packs"`

	ProposedSeeds     int `json:"proposed_seeds"`
	ProposedSeedsUsed int `json:"proposed_seeds_used"`

	WindowsWithEdits   int `json:"windows_with_edits"`
	EditedFiles        int `json:"edited_files"`
	EditedFilesInPack  int `json:"edited_files_in_pack"`
	PackFiles          int `json:"pack_files"`
	PackFilesUsed      int `json:"pack_files_used"`
	SearchesBeforeEdit int `json:"searches_before_edit"`
	SeedRereads        int `json:"seed_rereads"`
	Feedback           int `json:"feedback"`

	WithPack    ContextOutcome `json:"with_pack"`
	WithoutPack ContextOutcome `json:"without_pack"`
}

func (p *ContextPeriod) add(b ContextPeriod) {
	p.Streams += b.Streams
	p.StreamsWithPack += b.StreamsWithPack
	p.Packs += b.Packs
	p.ProsePacks += b.ProsePacks
	p.ProposedSeeds += b.ProposedSeeds
	p.ProposedSeedsUsed += b.ProposedSeedsUsed
	p.WindowsWithEdits += b.WindowsWithEdits
	p.EditedFiles += b.EditedFiles
	p.EditedFilesInPack += b.EditedFilesInPack
	p.PackFiles += b.PackFiles
	p.PackFilesUsed += b.PackFilesUsed
	p.SearchesBeforeEdit += b.SearchesBeforeEdit
	p.SeedRereads += b.SeedRereads
	p.Feedback += b.Feedback
	p.WithPack.add(b.WithPack)
	p.WithoutPack.add(b.WithoutPack)
}

// ContextCount is one lead and how often it was seen.
type ContextCount struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// ContextFeedback is one note an agent left with tool_feedback.
type ContextFeedback struct {
	At      time.Time `json:"at"`
	Tool    string    `json:"tool"`
	Missing string    `json:"missing,omitempty"`
	Noise   string    `json:"noise,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// ContextLeads are the agents' own pointers at what packs lack.
type ContextLeads struct {
	MissedFiles []ContextCount    `json:"missed_files"`
	Searches    []ContextCount    `json:"searches_after_pack"`
	Gaps        []ContextCount    `json:"gaps"`
	Feedback    []ContextFeedback `json:"feedback"`
}

// ContextReport is the whole report: the weeks oldest first, their total, and
// the leads.
type ContextReport struct {
	Since time.Time       `json:"since"`
	Until time.Time       `json:"until"`
	Weeks []ContextPeriod `json:"weeks"`
	Total ContextPeriod   `json:"total"`
	Leads ContextLeads    `json:"leads"`
}

// contextRow is one recorded call as the report reads it.
type contextRow struct {
	stream string
	tool   string
	at     time.Time
	ok     bool
	in     string
	out    string
}

// ContextReport reads every call in filter's workspace since filter.Since (the
// caller sets it) and builds the report, bucketed into UTC weeks starting on
// Monday. nil-safe.
func (d *DB) ContextReport(filter Filter, now time.Time) (ContextReport, error) {
	if d == nil {
		return ContextReport{}, nil
	}
	rows, err := d.contextRows(filter)
	if err != nil {
		return ContextReport{}, err
	}
	return buildContextReport(rows, filter.Since, now), nil
}

// contextRows reads the window's rows, the output only for outputTools.
//
//nolint:gosec // G202: the IN list is placeholders for constant tool names and where() binds every value
func (d *DB) contextRows(filter Filter) ([]contextRow, error) {
	where, args := filter.where()
	ph := strings.TrimSuffix(strings.Repeat("?,", len(outputTools)), ",")
	q := `SELECT session_id, logical_agent, tool, called_at, success, input_json,
	             CASE WHEN tool IN (` + ph + `) THEN output_text ELSE '' END
	      FROM tool_calls ` + where + ` ORDER BY session_id, logical_agent, called_at, id`
	all := make([]any, 0, len(outputTools)+len(args))
	for _, t := range outputTools {
		all = append(all, t)
	}
	all = append(all, args...)
	rs, err := d.db.Query(q, all...)
	if err != nil {
		return nil, fmt.Errorf("stats: context report: %w", err)
	}
	defer rs.Close()
	var out []contextRow
	for rs.Next() {
		var sid, agent, tool, in, outText string
		var at int64
		var ok bool
		if err := rs.Scan(&sid, &agent, &tool, &at, &ok, &in, &outText); err != nil {
			return nil, fmt.Errorf("stats: context report: %w", err)
		}
		out = append(out, contextRow{stream: sid + "\x00" + agent, tool: tool, at: time.UnixMilli(at), ok: ok, in: in, out: outText})
	}
	return out, rs.Err()
}

// weekStart is the Monday 00:00 UTC that starts t's week.
func weekStart(t time.Time) time.Time {
	t = t.UTC()
	d := (int(t.Weekday()) + 6) % 7
	return time.Date(t.Year(), t.Month(), t.Day()-d, 0, 0, 0, 0, time.UTC)
}

// reportBuilder accumulates the periods and leads across streams.
type reportBuilder struct {
	weeks    map[time.Time]*ContextPeriod
	missed   map[string]int
	searches map[string]int
	gaps     map[string]int
	feedback []ContextFeedback
}

func (b *reportBuilder) week(t time.Time) *ContextPeriod {
	k := weekStart(t)
	if b.weeks[k] == nil {
		b.weeks[k] = &ContextPeriod{Start: k}
	}
	return b.weeks[k]
}

func buildContextReport(rows []contextRow, since, now time.Time) ContextReport {
	b := &reportBuilder{weeks: map[time.Time]*ContextPeriod{}, missed: map[string]int{}, searches: map[string]int{}, gaps: map[string]int{}}
	for start := 0; start < len(rows); {
		end := start
		for end < len(rows) && rows[end].stream == rows[start].stream {
			end++
		}
		b.stream(rows[start:end])
		start = end
	}
	r := ContextReport{Since: since, Until: now}
	keys := make([]time.Time, 0, len(b.weeks))
	for k := range b.weeks {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	for _, k := range keys {
		r.Weeks = append(r.Weeks, *b.weeks[k])
		r.Total.add(*b.weeks[k])
	}
	if len(keys) > 0 {
		r.Total.Start = keys[0]
	}
	sort.SliceStable(b.feedback, func(i, j int) bool { return b.feedback[i].At.After(b.feedback[j].At) })
	if len(b.feedback) > contextReportMaxFeedback {
		b.feedback = b.feedback[:contextReportMaxFeedback]
	}
	r.Leads = ContextLeads{MissedFiles: topCounts(b.missed), Searches: topCounts(b.searches), Gaps: topCounts(b.gaps), Feedback: b.feedback}
	return r
}

// topCounts orders counts by frequency, then text, and keeps the first few.
func topCounts(m map[string]int) []ContextCount {
	out := make([]ContextCount, 0, len(m))
	for k, v := range m {
		out = append(out, ContextCount{Text: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > contextReportMaxLeads {
		out = out[:contextReportMaxLeads]
	}
	return out
}

// packWindow is the stretch of a stream from one pack to the next pack or the
// stream's end: what the agent did with what the pack gave it.
type packWindow struct {
	pack      packInfo
	at        time.Time
	edited    []string
	touched   []string
	edits     int
	searches  int
	rereads   int
	firstEdit bool
}

// streamState is one agent stream being read.
type streamState struct {
	outcome  ContextOutcome
	files    []string
	lastTest string
	window   *packWindow
	packs    int
}

// stream reads one agent stream's calls in order.
func (b *reportBuilder) stream(rows []contextRow) {
	s := &streamState{}
	for _, r := range rows {
		b.call(s, r)
	}
	b.closeWindow(s)
	w := b.week(rows[0].at)
	w.Streams++
	if s.packs > 0 {
		w.StreamsWithPack++
	}
	if s.outcome.Edits == 0 {
		return
	}
	s.outcome.Streams = 1
	s.outcome.FilesEdited = len(s.files)
	if s.lastTest == "pass" {
		s.outcome.EndedGreen = 1
	}
	if s.outcome.TestRuns > 0 {
		s.outcome.StreamsWithTests = 1
	}
	if s.packs > 0 {
		w.WithPack.add(s.outcome)
	} else {
		w.WithoutPack.add(s.outcome)
	}
}

func (b *reportBuilder) call(s *streamState, r contextRow) {
	switch {
	case r.tool == toolContextForTask:
		if r.ok {
			b.openWindow(s, r)
		}
	case r.tool == toolFeedback:
		b.feedbackNote(r)
	case editTools[r.tool]:
		b.edit(s, r)
	case readTools[r.tool]:
		s.read(r)
	case searchTools[r.tool]:
		b.search(s, r)
	case r.tool == toolUndoEdit:
		s.outcome.Undos++
	case r.tool == toolRunTask:
		s.testRun(r)
	}
}

// read notes the files a read after a pack touched, and re-reads of its seeds.
func (s *streamState) read(r contextRow) {
	w := s.window
	if w == nil || !r.ok {
		return
	}
	for _, p := range callPaths(r.in) {
		w.touched = append(w.touched, p)
		if containsPath(w.pack.seeds, p) {
			w.rereads++
		}
	}
}

// search counts a search after a pack and before its first edit, and keeps what
// it asked as a lead.
func (b *reportBuilder) search(s *streamState, r contextRow) {
	w := s.window
	if w == nil || w.firstEdit {
		return
	}
	w.searches++
	if q := searchText(r.in); q != "" {
		b.searches[clampLead(r.tool+": "+q)]++
	}
}

// testRun records a run_task test run's outcome.
func (s *streamState) testRun(r contextRow) {
	o := testOutcome(r.in, r.out)
	if o == "" {
		return
	}
	s.outcome.TestRuns++
	if o == "fail" {
		s.outcome.TestFailures++
	}
	s.lastTest = o
}

func (b *reportBuilder) edit(s *streamState, r contextRow) {
	s.outcome.Edits++
	if !r.ok {
		s.outcome.EditFailures++
		return
	}
	if present, n := newErrors(r.out); present {
		s.outcome.DiagnosedEdits++
		if n > 0 {
			s.outcome.NewErrorEdits++
		}
	}
	for _, p := range callPaths(r.in) {
		if !containsPath(s.files, p) {
			s.files = append(s.files, p)
		}
		if w := s.window; w != nil && !containsPath(w.edited, p) {
			w.edited = append(w.edited, p)
		}
	}
	if w := s.window; w != nil {
		w.edits++
		w.firstEdit = true
	}
}

// gapDigitsRe folds the counts inside a gap line, so the same gap with different
// numbers is one lead.
var gapDigitsRe = regexp.MustCompile(`\d+`)

func (b *reportBuilder) openWindow(s *streamState, r contextRow) {
	b.closeWindow(s)
	s.packs++
	p := parsePack(r.in, r.out)
	s.window = &packWindow{pack: p, at: r.at}
	w := b.week(r.at)
	w.Packs++
	if p.prose {
		w.ProsePacks++
	}
	inGaps := false
	for _, line := range strings.Split(r.out, "\n") {
		switch {
		case !strings.HasPrefix(line, " "):
			inGaps = line == "gaps:"
		case inGaps && strings.HasPrefix(line, "  - "):
			b.gaps[clampLead(gapDigitsRe.ReplaceAllString(strings.TrimPrefix(line, "  - "), "N"))]++
		}
	}
}

// closeWindow scores the open pack window against what followed it.
func (b *reportBuilder) closeWindow(s *streamState) {
	w := s.window
	if w == nil {
		return
	}
	s.window = nil
	p := b.week(w.at)
	used := append(append([]string(nil), w.touched...), w.edited...)
	for _, f := range w.pack.files {
		p.PackFiles++
		if containsPath(used, f) {
			p.PackFilesUsed++
		}
	}
	for _, f := range w.pack.proposed {
		p.ProposedSeeds++
		if containsPath(used, f) {
			p.ProposedSeedsUsed++
		}
	}
	p.SeedRereads += w.rereads
	if w.edits == 0 {
		return
	}
	p.WindowsWithEdits++
	p.SearchesBeforeEdit += w.searches
	for _, f := range w.edited {
		p.EditedFiles++
		if containsPath(w.pack.files, f) {
			p.EditedFilesInPack++
		} else {
			b.missed[clampLead(f)]++
		}
	}
}

func (b *reportBuilder) feedbackNote(r contextRow) {
	if !r.ok {
		return
	}
	b.week(r.at).Feedback++
	var in struct {
		Tool    string `json:"tool"`
		Missing string `json:"missing"`
		Noise   string `json:"noise"`
		Note    string `json:"note"`
	}
	if json.Unmarshal([]byte(r.in), &in) != nil {
		return
	}
	b.feedback = append(b.feedback, ContextFeedback{At: r.at, Tool: clampLead(in.Tool), Missing: clampLead(in.Missing), Noise: clampLead(in.Noise), Note: clampLead(in.Note)})
}

// clampLead keeps a lead to one bounded line.
func clampLead(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= contextReportLeadBytes {
		return s
	}
	cut := contextReportLeadBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
