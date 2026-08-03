package mcp_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// These tests run alias-bearing calls against the REAL tool schemas through
// the full tools/call dispatch (Serve → resolveToolArgs → Execute) — not the
// hand-written schemas of argalias_test.go. They are the parity guard for
// "the alias table works in the field": a real schema that parseShape cannot
// guard (silently disabling aliases for that tool) or an alias entry that
// does not resolve against the tool's actual properties fails here while the
// synthetic-schema tests still pass.

// realToolServer registers the real filesystem tools plus run_task with a
// slot-echoing resolver, so a test can prove the daemon-side rewrite fired.
func realToolServer() *mcp.Server {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(tools.NewReadFile(nil))
	s.Register(tools.NewEditFile(tools.WriteDeps{}))
	s.Register(tools.NewWriteFile(tools.WriteDeps{}))
	s.Register(tools.NewDeleteFile(tools.WriteDeps{}))
	s.Register(tools.NewRenameFile(tools.WriteDeps{}))
	s.Register(tools.NewFindFiles(nil))
	s.Register(tools.NewFindReplace())
	s.Register(tools.NewSearchInFiles(nil, nil, nil, 0))
	s.Register(tools.NewTasks(tools.WriteDeps{}, func(slot, _ string) (tools.TaskCommand, error) {
		return tools.TaskCommand{}, fmt.Errorf("resolver saw slot=%s", slot)
	}))
	return s
}

func callTool(t *testing.T, s *mcp.Server, name, args string) string {
	t.Helper()
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
	resps := serveOn(t, s, req)
	return toolText(resultByID(t, resps, 1))
}

// TestToolsCall_RealSchema_PathAliasOnReadFile is the exact call reported from
// the field: read_file({"path": ...}) must be interpreted as file_path and
// succeed, never rejected with `missing required parameter "file_path"`.
func TestToolsCall_RealSchema_PathAliasOnReadFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(f, []byte("hello aliased read\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := callTool(t, realToolServer(), "read_file", fmt.Sprintf(`{"path":%q}`, f))
	if !strings.Contains(text, `interpreted "path" as "file_path"`) {
		t.Errorf("missing alias notice; got: %s", text)
	}
	if !strings.Contains(text, "hello aliased read") {
		t.Errorf("read did not return file content; got: %s", text)
	}
}

// TestToolsCall_RealSchema_TaskAliasOnRunTask proves {"task": "lint"} reaches
// run_task's resolver as slot=lint through the real schema.
func TestToolsCall_RealSchema_TaskAliasOnRunTask(t *testing.T) {
	text := callTool(t, realToolServer(), "run_task", `{"task":"lint"}`)
	if !strings.Contains(text, "resolver saw slot=lint") {
		t.Errorf("task alias did not reach the resolver as slot; got: %s", text)
	}
}

// TestToolsCall_RealSchemas_CommonAliases sweeps the field-reported alias
// spellings across the real tools. Success cases must carry the alias notice;
// for calls that fail later (e.g. a missing file) the assertion is that the
// failure is NOT the argument guard's — the rewrite must already have happened.
func TestToolsCall_RealSchemas_CommonAliases(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		tool     string
		args     string
		wantSubs []string
	}{
		{"read_file", fmt.Sprintf(`{"filename":%q}`, f), []string{`interpreted "filename" as "file_path"`, "alpha"}},
		{"read_file", fmt.Sprintf(`{"file":%q}`, f), []string{`interpreted "file" as "file_path"`, "alpha"}},
		{"read_file", fmt.Sprintf(`{"filepath":%q}`, f), []string{`interpreted "filepath" as "file_path"`, "alpha"}},
		{"write_file", fmt.Sprintf(`{"path":%q,"text":"beta"}`, filepath.Join(dir, "b.txt")), []string{`interpreted "path" as "file_path"`, `interpreted "text" as "content"`}},
		{"edit_file", fmt.Sprintf(`{"path":%q,"edits":[{"old_str":"alpha","new_str":"gamma"}]}`, f), []string{`interpreted "path" as "file_path"`, `interpreted "edits[].old_str" as "old_string"`}},
		// list_files is a retired NAME served by find_files: the tool-name alias
		// resolves first, then the parameter alias rewrites dir onto find_files'
		// own "path" — both layers in one call.
		{"list_files", fmt.Sprintf(`{"dir":%q}`, dir), []string{`interpreted "dir" as "path"`, "a.txt"}},
		// The edit_file case above already rewrote alpha → gamma in a.txt.
		{"search_in_files", fmt.Sprintf(`{"path":%q,"query":"gamma"}`, dir), []string{`interpreted "query" as "pattern"`, "a.txt"}},
		{"rename_file", fmt.Sprintf(`{"source":%q,"destination":%q}`, f, filepath.Join(dir, "c.txt")), []string{`interpreted "source" as "from"`, `interpreted "destination" as "to"`}},
		{"delete_file", fmt.Sprintf(`{"filepath":%q}`, filepath.Join(dir, "c.txt")), []string{`interpreted "filepath" as "file_path"`}},
	}
	s := realToolServer()
	for _, tc := range testCases {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			text := callTool(t, s, tc.tool, tc.args)
			if strings.Contains(text, "missing required parameter") || strings.Contains(text, "unknown parameter") {
				t.Fatalf("argument guard rejected an aliased call against the real schema: %s", text)
			}
			for _, want := range tc.wantSubs {
				if !strings.Contains(text, want) {
					t.Errorf("response missing %q; got: %s", want, text)
				}
			}
		})
	}
}

// TestToolsCall_RealSchema_NLinesOnReadFile is the exact failure reported from
// the field: read_file({"n_lines": …}) must be interpreted as limit and
// succeed, never rejected with `unknown parameter "n_lines"`.
func TestToolsCall_RealSchema_NLinesOnReadFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(f, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := callTool(t, realToolServer(), "read_file", fmt.Sprintf(`{"path":%q,"n_lines":2}`, f))
	if !strings.Contains(text, `interpreted "n_lines" as "limit"`) {
		t.Errorf("missing alias notice; got: %s", text)
	}
	if !strings.Contains(text, "two") || strings.Contains(text, "three") {
		t.Errorf("limit window not honoured; got: %s", text)
	}
}

// TestToolsCall_RealSchema_InvertCaseFlagOnSearch is the second field failure:
// a grep-style -i flag must invert into case_sensitive:false instead of
// hard-failing as an unknown parameter. The behavioural proof uses
// find_replace, which honours an explicit case_sensitive:false as
// force-insensitive (search_in_files collapses it into its smart-case path —
// there the transform still lands and is noted, the outcome is just unchanged
// for a mixed-case pattern). The control call (no -i, mixed-case pattern)
// proves the transform changed the outcome: smart-case makes "Alpha"
// case-sensitive, so ALPHA only matches once -i forces case-insensitivity.
func TestToolsCall_RealSchema_InvertCaseFlagOnSearch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("ALPHA beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := realToolServer()
	control := callTool(t, s, "find_replace", fmt.Sprintf(`{"path":%q,"pattern":"Alpha","replacement":"omega"}`, f))
	if !strings.Contains(control, "0 file(s)") {
		t.Fatalf("control call unexpectedly matched (smart-case should be case-sensitive here): %s", control)
	}
	text := callTool(t, s, "find_replace", fmt.Sprintf(`{"path":%q,"pattern":"Alpha","replacement":"omega","-i":true}`, f))
	if !strings.Contains(text, `interpreted "-i" as "case_sensitive" (inverted value)`) {
		t.Errorf("missing transform notice; got: %s", text)
	}
	if !strings.Contains(text, "1 file(s), 1 replacement(s)") {
		t.Errorf("case-insensitive match not applied; got: %s", text)
	}
	// The -i call also runs through search_in_files: no hard failure, notice
	// present (its smart-case path keeps a mixed-case pattern sensitive — an
	// existing tool behaviour, not something the alias layer overrides).
	text = callTool(t, s, "search_in_files", fmt.Sprintf(`{"path":%q,"pattern":"alpha","-i":true}`, dir))
	if !strings.Contains(text, `interpreted "-i" as "case_sensitive" (inverted value)`) {
		t.Errorf("missing transform notice on search_in_files; got: %s", text)
	}
	if !strings.Contains(text, "ALPHA") {
		t.Errorf("lowercase -i search should match ALPHA via case-insensitivity; got: %s", text)
	}
}

// TestToolsCall_RealSchema_PreviewOnFindReplace: preview:true is a constant
// flag alias for dry_run:true.
func TestToolsCall_RealSchema_PreviewOnFindReplace(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := callTool(t, realToolServer(), "find_replace", fmt.Sprintf(`{"path":%q,"pattern":"alpha","replacement":"omega","preview":true}`, f))
	if !strings.Contains(text, `interpreted "preview" as "dry_run" (forced true)`) {
		t.Errorf("missing transform notice; got: %s", text)
	}
	body, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "alpha\n" {
		t.Errorf("preview wrote to the file; body = %q", body)
	}
}

// TestToolsCall_RealSchemas_Phase2Aliases sweeps the remaining new table rows
// across the real tools. As in the common-aliases sweep, the assertion is the
// alias notice plus the observable effect of the rewritten call.
// phase2Fixture builds the directory tree the phase-2 sweep exercises and
// returns the directory, the file edit_file rewrites, and the path write_file
// creates.
func phase2Fixture(t *testing.T) (dir, editFile, writeFile string) {
	t.Helper()
	dir = t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(dir, "a.go"):     "package a\n",
		filepath.Join(dir, "b.txt"):    "text\n",
		filepath.Join(dir, ".secret"):  "hidden\n",
		filepath.Join(sub, "deep.txt"): "deep\n",
	}
	editFile = filepath.Join(dir, "edit.txt")
	files[editFile] = "alpha\n"
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, editFile, filepath.Join(dir, "written.txt")
}

func TestToolsCall_RealSchemas_Phase2Aliases(t *testing.T) {
	dir, editFile, writeFile := phase2Fixture(t)

	testCases := []struct {
		name       string
		tool       string
		args       string
		wantSubs   []string
		notWantSub []string
	}{
		{
			name:     "limit → max_results on search_in_files",
			tool:     "search_in_files",
			args:     fmt.Sprintf(`{"path":%q,"pattern":"a","limit":1}`, dir),
			wantSubs: []string{`interpreted "limit" as "max_results"`},
		},
		{
			name:     "max_matches → max_results on search_in_files",
			tool:     "search_in_files",
			args:     fmt.Sprintf(`{"path":%q,"pattern":"a","max_matches":1}`, dir),
			wantSubs: []string{`interpreted "max_matches" as "max_results"`},
		},
		{
			name:     "hidden → include_hidden on search_in_files",
			tool:     "search_in_files",
			args:     fmt.Sprintf(`{"path":%q,"pattern":"hidden","hidden":true}`, dir),
			wantSubs: []string{`interpreted "hidden" as "include_hidden"`, ".secret"},
		},
		{
			name:       "depth → max_depth on find_files (via the list_files name alias)",
			tool:       "list_files",
			args:       fmt.Sprintf(`{"dir":%q,"depth":1}`, dir),
			wantSubs:   []string{`interpreted "depth" as "max_depth"`, "a.go"},
			notWantSub: []string{"deep.txt"},
		},
		{
			name:     "sort/hidden → sort_by/include_hidden on find_files (via the list_directory name alias)",
			tool:     "list_directory",
			args:     fmt.Sprintf(`{"path":%q,"sort":"name","hidden":true}`, dir),
			wantSubs: []string{`interpreted "sort" as "sort_by"`, `interpreted "hidden" as "include_hidden"`, ".secret"},
		},
		{
			name:       "ext → extension on find_files",
			tool:       "find_files",
			args:       fmt.Sprintf(`{"pattern":"*","path":%q,"ext":"go"}`, dir),
			wantSubs:   []string{`interpreted "ext" as "extension"`, "a.go"},
			notWantSub: []string{"b.txt"},
		},
		{
			name:     "data → content on write_file",
			tool:     "write_file",
			args:     fmt.Sprintf(`{"path":%q,"data":"payload"}`, writeFile),
			wantSubs: []string{`interpreted "data" as "content"`},
		},
		{
			name:     "changes → edits on edit_file",
			tool:     "edit_file",
			args:     fmt.Sprintf(`{"path":%q,"changes":[{"old_string":"alpha","new_string":"omega"}]}`, editFile),
			wantSubs: []string{`interpreted "changes" as "edits"`},
		},
	}
	s := realToolServer()
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			text := callTool(t, s, tc.tool, tc.args)
			if strings.Contains(text, "missing required parameter") || strings.Contains(text, "unknown parameter") {
				t.Fatalf("argument guard rejected an aliased call against the real schema: %s", text)
			}
			for _, want := range tc.wantSubs {
				if !strings.Contains(text, want) {
					t.Errorf("response missing %q; got: %s", want, text)
				}
			}
			for _, notWant := range tc.notWantSub {
				if strings.Contains(text, notWant) {
					t.Errorf("response unexpectedly contains %q; got: %s", notWant, text)
				}
			}
		})
	}

	// The write_file and edit_file cases above must have landed for real.
	if body, err := os.ReadFile(writeFile); err != nil || string(body) != "payload" {
		t.Errorf("write_file(data) body = %q, %v", body, err)
	}
	if body, err := os.ReadFile(editFile); err != nil || string(body) != "omega\n" {
		t.Errorf("edit_file(changes) body = %q, %v", body, err)
	}
}

// TestToolsCall_RealSchema_StringCoercion: string values for the integer
// offset/limit are coerced against the real read_file schema, with notices.
func TestToolsCall_RealSchema_StringCoercion(t *testing.T) {
	f := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(f, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := callTool(t, realToolServer(), "read_file", fmt.Sprintf(`{"path":%q,"offset":"2","limit":"1"}`, f))
	for _, want := range []string{`coerced "offset" from string to integer`, `coerced "limit" from string to integer`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing coercion notice %q; got: %s", want, text)
		}
	}
	if !strings.Contains(text, "two") || strings.Contains(text, "three") {
		t.Errorf("coerced window not honoured; got: %s", text)
	}
}

// TestToolsCall_RealSchema_DryRunStringCoercion: "false" for the boolean
// dry_run coerces against the real find_replace schema (whose dry_run defaults
// to true) and therefore writes for real — the string would otherwise fail
// the tool's json.Unmarshal outright.
func TestToolsCall_RealSchema_DryRunStringCoercion(t *testing.T) {
	f := filepath.Join(t.TempDir(), "edit.txt")
	if err := os.WriteFile(f, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := callTool(t, realToolServer(), "find_replace", fmt.Sprintf(`{"path":%q,"pattern":"alpha","replacement":"omega","dry_run":"false"}`, f))
	if !strings.Contains(text, `coerced "dry_run" from string to boolean`) {
		t.Errorf("missing coercion notice; got: %s", text)
	}
	body, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "omega\n" {
		t.Errorf(`dry_run:"false" did not write; body = %q`, body)
	}
}

// TestToolsCall_RealSchema_CoercionStillRejectsGarbage: a string that does not
// parse as the declared type is NOT coerced and still fails the tool's decode —
// with the tool's decode error, not the argument guard's unknown-parameter one.
func TestToolsCall_RealSchema_CoercionStillRejectsGarbage(t *testing.T) {
	f := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(f, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := callTool(t, realToolServer(), "read_file", fmt.Sprintf(`{"path":%q,"offset":"abc"}`, f))
	if strings.Contains(text, "unknown parameter") {
		t.Errorf("guard rejected a declared parameter; got: %s", text)
	}
	if !strings.Contains(text, "invalid arguments") {
		t.Errorf("expected the tool's decode error for offset:\"abc\"; got: %s", text)
	}
}
