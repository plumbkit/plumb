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
	s.Register(tools.NewListFiles(nil))
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
		{"list_files", fmt.Sprintf(`{"dir":%q}`, dir), []string{`interpreted "dir" as "root"`, "a.txt"}},
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
