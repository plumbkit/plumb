package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestReadSymbol_SingleMatch(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name\n}\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Greet",
				Kind: protocol.SKFunction,
				Range: protocol.Range{
					Start: protocol.Position{Line: 2},
					End:   protocol.Position{Line: 4},
				},
			},
		},
	}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Greet"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	for _, want := range []string{"plumb-read", "Greet", "Function", "return"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestReadSymbol_DottedName(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\ntype Server struct{}\n\nfunc (s *Server) Start() {}\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Server",
				Kind: protocol.SKStruct,
				Range: protocol.Range{
					Start: protocol.Position{Line: 2},
					End:   protocol.Position{Line: 2},
				},
				Children: []protocol.DocumentSymbol{
					{
						Name: "Start",
						Kind: protocol.SKMethod,
						Range: protocol.Range{
							Start: protocol.Position{Line: 4},
							End:   protocol.Position{Line: 4},
						},
					},
				},
			},
		},
	}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Server.Start"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "Start") {
		t.Errorf("expected Start in output:\n%s", out)
	}
	if !strings.Contains(out, "Start()") {
		t.Errorf("expected source line in output:\n%s", out)
	}
}

func TestReadSymbol_MultipleMatches(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nfunc Run() {}\n\nfunc Run() error { return nil }\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "Run", Kind: protocol.SKFunction, Range: protocol.Range{
				Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 2},
			}},
			{Name: "Run", Kind: protocol.SKFunction, Range: protocol.Range{
				Start: protocol.Position{Line: 4}, End: protocol.Position{Line: 4},
			}},
		},
	}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Run"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "2 matches") {
		t.Errorf("expected '2 matches' for ambiguous name:\n%s", out)
	}
}

func TestReadSymbol_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{docSymbols: nil}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Missing"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No symbol") {
		t.Errorf("expected no-symbol message:\n%s", out)
	}
}

func TestReadSymbol_MissingPath(t *testing.T) {
	tool := tools.NewReadSymbol(&mockLSP{}, nil, time.Minute, 0, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"name": "Greet"})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestReadSymbol_MissingName(t *testing.T) {
	tool := tools.NewReadSymbol(&mockLSP{}, nil, time.Minute, 0, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": "/some/file.go"})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

// TestReadSymbol_EditLaneHint_ClaudeCode verifies read_symbol — itself a
// read-before-edit precursor — appends the edit_file call-to-action as a second
// header comment line for Claude Code, carrying the exact mtime. The plumb-read
// header must remain the first line so the shared header parsers still work.
func TestReadSymbol_EditLaneHint_ClaudeCode(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name\n}\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{
		{Name: "Greet", Kind: protocol.SKFunction, Range: protocol.Range{
			Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 4},
		}},
	}}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker()).
		WithClient(func() string { return "claude-code" })
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Greet"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.HasPrefix(out, "# plumb-read mtime=") {
		t.Fatalf("plumb-read header must remain the first line, got:\n%s", out)
	}
	lines := strings.SplitN(out, "\n", 3)
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "# ") {
		t.Fatalf("expected the edit-lane hint as the second comment line, got:\n%s", out)
	}
	for _, want := range []string{"edit_file", "native Edit", "expected_mtime"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("hint line missing %q: %q", want, lines[1])
		}
	}
	// The mtime in the hint must equal the mtime in the header (copy-paste ready).
	if !strings.Contains(lines[1], readSymbolHeaderMtime(lines[0])) {
		t.Errorf("hint mtime should match header mtime %q: %q", readSymbolHeaderMtime(lines[0]), lines[1])
	}
	// Symbol body must still be present after the header block.
	if !strings.Contains(out, "return") {
		t.Errorf("symbol body missing from output:\n%s", out)
	}
}

// TestReadSymbol_NoEditLaneHint_OtherClients verifies the hint is suppressed for
// clients without the native-edit conflict (and when no client is wired), so
// their read_symbol output stays lean.
func TestReadSymbol_NoEditLaneHint_OtherClients(t *testing.T) {
	cases := []struct {
		name   string
		client func() string
	}{
		{"nil client", nil},
		{"claude desktop", func() string { return "claude-ai" }},
		{"vscode", func() string { return "vscode" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "p.go")
			if err := os.WriteFile(path, []byte("package p\n\nfunc Greet() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{
				{Name: "Greet", Kind: protocol.SKFunction, Range: protocol.Range{
					Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 2},
				}},
			}}
			tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker())
			if c.client != nil {
				tool = tool.WithClient(c.client)
			}
			raw, _ := json.Marshal(map[string]any{"path": path, "name": "Greet"})
			out, err := tool.Execute(context.Background(), raw)
			if err != nil {
				t.Fatalf("read_symbol: %v", err)
			}
			if strings.Contains(out, "native Edit") || strings.Contains(out, "expected_mtime") {
				t.Errorf("non-conflict client must not get the edit-lane hint:\n%s", out)
			}
		})
	}
}

// readSymbolHeaderMtime pulls the mtime= value out of a plumb-read header line.
func readSymbolHeaderMtime(header string) string {
	const key = "mtime="
	i := strings.Index(header, key)
	if i < 0 {
		return ""
	}
	rest := header[i+len(key):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return rest[:j]
	}
	return rest
}

// TestReadSymbol_NotFound_Suggests verifies a near-miss query gets a "did you
// mean?" fragment built from the already-fetched symbol list.
func TestReadSymbol_NotFound_Suggests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "fsWatcher", Kind: protocol.SKStruct},
			{Name: "startIndexer", Kind: protocol.SKFunction},
		},
	}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Watcher"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"No symbol", "Did you mean:", "`fsWatcher`"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "startIndexer") {
		t.Errorf("unrelated symbol suggested:\n%s", out)
	}
}

// TestReadSymbol_NotFound_NoSuggestionsPlainMessage verifies the not-found
// message stays byte-identical when nothing in the file is a near miss.
func TestReadSymbol_NotFound_NoSuggestionsPlainMessage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "startIndexer", Kind: protocol.SKFunction},
		},
	}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Zebra"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `No symbol named "Zebra" in ` + path + "."
	if out != want {
		t.Errorf("message not byte-identical:\ngot  %q\nwant %q", out, want)
	}
}

// TestReadSymbol_NotFound_OutsideWorkspaceHint verifies the out-of-workspace
// hint still renders on the not-found path alongside the suggestions.
func TestReadSymbol_NotFound_OutsideWorkspaceHint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "fsWatcher", Kind: protocol.SKStruct},
		},
	}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, 0, tools.NewReadTracker()).
		WithOutsideLabel(func(string) string { return "GOMODCACHE" })
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Watcher"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"Did you mean:", "`fsWatcher`", "outside the workspace"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
