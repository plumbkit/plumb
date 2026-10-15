package stats

// context_report_parse.go — reading one recorded tool call the way the context
// report needs it: which files a call read or changed, what it searched for,
// how a test run or an edit's diagnostics came out, and which files a
// context_for_task pack named. Everything here reads text plumb itself wrote
// (the input JSON a call carried, the output it returned), so each format is
// pinned by a test that renders the producer's real output (internal/cli).

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Tool names the report classifies. Kept here, not imported, because stats sits
// below internal/tools; the cli-level test pins them against the registry.
const (
	toolContextForTask = "context_for_task"
	toolFeedback       = "tool_feedback"
	toolRunTask        = "run_task"
	toolUndoEdit       = "undo_edit"
)

// editTools change files. A call is an edit attempt whatever its outcome.
var editTools = map[string]bool{
	"edit_file": true, "write_file": true, "transaction_apply": true, "replace_symbol_body": true,
	"insert_after_symbol": true, "insert_before_symbol": true, "rename_symbol": true, "find_replace": true,
	"move_symbol": true, "safe_delete_symbol": true, "rename_file": true, "delete_file": true,
}

// previewByDefault are the edit tools whose dry_run defaults to true: a call to
// one changed nothing unless it said dry_run false.
var previewByDefault = map[string]bool{
	"find_replace": true, "move_symbol": true, "rename_symbol": true, "replace_symbol_body": true,
	"insert_after_symbol": true, "insert_before_symbol": true, "safe_delete_symbol": true,
}

// PreviewByDefault reports whether tool's dry_run defaults to true, for the
// tools' own test to hold this list to their schemas (stats cannot import tools).
func PreviewByDefault(tool string) bool { return previewByDefault[tool] }

// isPreview reports whether an edit call only previewed its change.
func isPreview(tool, inputJSON string) bool {
	var in struct {
		DryRun *bool `json:"dry_run"`
	}
	_ = json.Unmarshal([]byte(inputJSON), &in)
	if in.DryRun != nil {
		return *in.DryRun
	}
	return previewByDefault[tool]
}

// editedPaths are the files an edit call names. find_replace's path may be a
// directory, and which files it changed is in no input, so it names none.
func editedPaths(tool, inputJSON string) []string {
	if tool == "find_replace" {
		return nil
	}
	return callPaths(inputJSON)
}

// readTools read source.
var readTools = map[string]bool{
	"read_file": true, "read_symbol": true, "read_multiple_files": true, "file_outline": true,
}

// searchTools look for code; the report keeps what they were asked.
var searchTools = map[string]bool{
	"search_in_files": true, "workspace_search": true, "topology_search": true, "find_files": true,
	"workspace_symbols": true, "find_references": true, "get_definition": true, "topology_explore": true,
	"structural_query": true, "call_hierarchy": true,
}

// outputTools are the tools whose recorded output the report reads; the query
// leaves every other row's output behind.
var outputTools = []string{
	toolContextForTask, toolRunTask, "edit_file", "write_file", "transaction_apply",
	"replace_symbol_body", "insert_after_symbol", "insert_before_symbol",
}

// callPaths returns the file paths a call's input names, slash-separated, with
// any file:// scheme removed.
func callPaths(inputJSON string) []string {
	var in map[string]any
	if json.Unmarshal([]byte(inputJSON), &in) != nil {
		return nil
	}
	var out []string
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, strings.ReplaceAll(strings.TrimPrefix(s, "file://"), `\`, "/"))
		}
	}
	for _, k := range []string{"file_path", "path", "uri", "from", "to", "source_path", "source_uri", "destination_uri"} {
		add(in[k])
	}
	for _, k := range []string{"paths", "files"} {
		if list, ok := in[k].([]any); ok {
			for _, v := range list {
				add(v)
			}
		}
	}
	if ops, ok := in["operations"].([]any); ok {
		for _, op := range ops {
			if m, ok := op.(map[string]any); ok {
				add(m["file_path"])
			}
		}
	}
	return out
}

// searchText is what a search call asked for, "" when it names nothing.
func searchText(inputJSON string) string {
	var in map[string]any
	if json.Unmarshal([]byte(inputJSON), &in) != nil {
		return ""
	}
	for _, k := range []string{"query", "pattern", "symbol_name", "symbol", "name"} {
		if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// testOutcome classifies a run_task call: "pass", "fail", or "" when it was not
// a test run or its outcome is not on record. run_task ends its output with
// "→ ok" or "→ stopped: …".
func testOutcome(inputJSON, output string) string {
	var in struct {
		Slot string `json:"slot"`
	}
	if json.Unmarshal([]byte(inputJSON), &in) != nil || in.Slot != "test" {
		return ""
	}
	tail := output
	if i := strings.LastIndex(output, "\n→ "); i >= 0 {
		tail = output[i+1:]
	}
	switch {
	case strings.HasPrefix(tail, "→ ok"):
		return "pass"
	case strings.HasPrefix(tail, "→ stopped"):
		return "fail"
	}
	return ""
}

// diagDeltaPrefix starts the machine-readable diagnostics line an edit appends
// (internal/tools/post_write_diag_delta.go).
const diagDeltaPrefix = "diagnostics delta: "

// newErrors reads an edit's diagnostics delta: whether the output carries a
// confirmed (fresh) one, and how many new errors (not warnings) it reports, in
// the edited file and across files. An unconfirmed delta carries no findings and
// is not a diagnosis.
func newErrors(output string) (present bool, n int) {
	i := strings.LastIndex(output, diagDeltaPrefix)
	if i < 0 {
		return false, 0
	}
	line := output[i+len(diagDeltaPrefix):]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	var d struct {
		Fresh          bool                        `json:"fresh"`
		NewErrors      []struct{ Severity string } `json:"new_errors"`
		CrossNewErrors []struct{ Severity string } `json:"cross_file_new_errors"`
	}
	if json.Unmarshal([]byte(line), &d) != nil || !d.Fresh {
		return false, 0
	}
	for _, e := range append(d.NewErrors, d.CrossNewErrors...) {
		if e.Severity == "" || e.Severity == "error" {
			n++
		}
	}
	return true, n
}

// Pack line shapes (internal/tools/context_render.go and its siblings):
//
//	file <path> [lang]…
//	symbol <selector> — <path>:<line> <kind>…
//	[e3 extractor] <name> — <path>:<line> <kind>…
var (
	packFileSeedRe = regexp.MustCompile(`^  file (\S+)`)
	packPathLineRe = regexp.MustCompile(`^  (?:symbol |\[e\d [^\]]*\] ).*? — (\S+?):\d+ `)
)

// Origins prose discovery stamps on a proposed seed (internal/tools
// context_discover.go).
const (
	packOriginNamed = "— named in the task"
	packOriginFound = "— found by search on:"
)

// packInfo is what the report needs from one rendered pack.
type packInfo struct {
	files    []string // seed and related files, in order, without repeats
	seeds    []string // seed files
	proposed []string // seed files prose discovery proposed
	prose    bool     // the request gave no files or symbols
}

// parsePack reads a pack's input and output.
func parsePack(inputJSON, output string) packInfo {
	var in struct {
		Files   []string `json:"files"`
		Symbols []string `json:"symbols"`
	}
	_ = json.Unmarshal([]byte(inputJSON), &in)
	p := packInfo{prose: len(in.Files)+len(in.Symbols) == 0}
	seen := map[string]bool{}
	inSeeds := false
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, " ") {
			inSeeds = strings.HasPrefix(line, "seeds (")
			continue
		}
		path := ""
		if m := packFileSeedRe.FindStringSubmatch(line); m != nil {
			path = m[1]
		} else if m := packPathLineRe.FindStringSubmatch(line); m != nil {
			path = m[1]
		}
		if path == "" {
			continue
		}
		if !seen[path] {
			seen[path] = true
			p.files = append(p.files, path)
		}
		if inSeeds && (strings.HasPrefix(line, "  file ") || strings.HasPrefix(line, "  symbol ")) {
			p.seeds = append(p.seeds, path)
			if strings.Contains(line, packOriginNamed) || strings.Contains(line, packOriginFound) {
				p.proposed = append(p.proposed, path)
			}
		}
	}
	return p
}

// samePath reports whether two recorded paths name the same file: equal, or one
// a slash-bounded suffix of the other (a root-relative pack path against an
// absolute or differently rooted input path).
func samePath(a, b string) bool {
	a, b = strings.TrimPrefix(a, "./"), strings.TrimPrefix(b, "./")
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

func containsPath(list []string, p string) bool {
	for _, q := range list {
		if samePath(q, p) {
			return true
		}
	}
	return false
}

// The exported forms below exist so the producers' own tests (internal/tools)
// can pin the formats the report reads against the text they really emit.

// PackFilesForReport is parsePack's answer for one recorded pack: its files, its
// seed files, the seed files prose discovery proposed, and whether the request
// was prose only.
func PackFilesForReport(inputJSON, output string) (files, seeds, proposed []string, prose bool) {
	p := parsePack(inputJSON, output)
	return p.files, p.seeds, p.proposed, p.prose
}

// EditNewErrorsForReport is newErrors, for the delta's producer to pin.
func EditNewErrorsForReport(output string) (present bool, n int) { return newErrors(output) }

// TestOutcomeForReport is testOutcome, for run_task's producer to pin.
func TestOutcomeForReport(inputJSON, output string) string { return testOutcome(inputJSON, output) }
