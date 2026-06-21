package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// writeFixture creates path under t.TempDir(), writes content, returns the
// absolute path and matching file:// URI.
func writeFixture(t *testing.T, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path, "file://" + path
}

// symbolAt builds a DocumentSymbol whose Range covers the given lines.
// charsOnEndLine is the column of the last byte of the symbol.
func symbolAt(name string, startLine, endLine, charsOnEndLine uint32) protocol.DocumentSymbol {
	return protocol.DocumentSymbol{
		Name: name,
		Kind: protocol.SKFunction,
		Range: protocol.Range{
			Start: protocol.Position{Line: startLine, Character: 0},
			End:   protocol.Position{Line: endLine, Character: charsOnEndLine},
		},
		SelectionRange: protocol.Range{
			Start: protocol.Position{Line: startLine, Character: 5},
			End:   protocol.Position{Line: startLine, Character: 8},
		},
	}
}

func TestReplaceSymbolBody_IncludeDocComment(t *testing.T) {
	// Layout (0-indexed lines):
	//  0: "// Foo doc."
	//  1: "func Foo() {}"
	src := "// Foo doc.\nfunc Foo() {}\n"
	path, uri := writeFixture(t, "main.go", src)

	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 1, 1, 13)}}
	tool := tools.NewReplaceSymbolBody(mock, 0)
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"uri":                 uri,
		"name_path":           "Foo",
		"content":             "// Foo v2 doc.\nfunc Foo() { return }",
		"dry_run":             &dryRun,
		"include_doc_comment": true,
	})

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "// Foo v2 doc.\nfunc Foo() { return }\n"
	if string(got) != want {
		t.Errorf("file mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestReplaceSymbolBody_DefaultLeavesDocComment(t *testing.T) {
	src := "// Foo doc.\nfunc Foo() {}\n"
	path, uri := writeFixture(t, "main.go", src)

	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 1, 1, 13)}}
	tool := tools.NewReplaceSymbolBody(mock, 0)
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"uri":       uri,
		"name_path": "Foo",
		"content":   "func Foo() { return }",
		"dry_run":   &dryRun,
		// include_doc_comment omitted → false → comment stays as-is
	})

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "// Foo doc.\nfunc Foo() { return }\n"
	if string(got) != want {
		t.Errorf("file mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestSafeDeleteSymbol_IncludeDocComment(t *testing.T) {
	src := "// Foo doc.\nfunc Foo() {}\n"
	path, uri := writeFixture(t, "main.go", src)

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 1, 1, 13)},
		locations:  nil, // no external references → deletion proceeds
	}
	tool := tools.NewSafeDeleteSymbol(mock, 0)
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"uri":                 uri,
		"name_path":           "Foo",
		"dry_run":             &dryRun,
		"include_doc_comment": true,
	})

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "\n" // both lines deleted, file end newline remains
	if string(got) != want {
		t.Errorf("file mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestInsertBeforeSymbol_IncludeDocCommentSkipsOverExistingDoc(t *testing.T) {
	// Inserting a new function (with its own doc) above Foo. With the flag,
	// the new content lands above the existing doc comment, not between
	// the doc comment and func Foo.
	src := "// Foo doc.\nfunc Foo() {}\n"
	path, uri := writeFixture(t, "main.go", src)

	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 1, 1, 13)}}
	tool := tools.NewInsertBeforeSymbol(mock, 0)
	dryRun := false
	newBlock := "// Bar doc.\nfunc Bar() {}\n\n"
	args, _ := json.Marshal(map[string]any{
		"uri":                 uri,
		"name_path":           "Foo",
		"content":             newBlock,
		"dry_run":             &dryRun,
		"include_doc_comment": true,
	})

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "// Bar doc.\nfunc Bar() {}\n\n// Foo doc.\nfunc Foo() {}\n"
	if string(got) != want {
		t.Errorf("file mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestReplaceSymbolBody_IncludeDocComment_MultiLineBlock(t *testing.T) {
	// JSDoc/JavaDoc-style /** ... */ block comment.
	src := "/**\n * Foo does things.\n */\nfunc Foo() {}\n"
	path, uri := writeFixture(t, "main.go", src)

	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 3, 3, 13)}}
	tool := tools.NewReplaceSymbolBody(mock, 0)
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"uri":                 uri,
		"name_path":           "Foo",
		"content":             "// new Foo.\nfunc Foo() { return }",
		"dry_run":             &dryRun,
		"include_doc_comment": true,
	})

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "// new Foo.\nfunc Foo() { return }\n"
	if string(got) != want {
		t.Errorf("file mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestReplaceSymbolBody_IncludeDocComment_NoCommentAbove(t *testing.T) {
	// When the flag is on but there's no comment block, behavior matches
	// the default — the symbol range alone is replaced.
	src := "package main\n\nfunc Foo() {}\n"
	path, uri := writeFixture(t, "main.go", src)

	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
	tool := tools.NewReplaceSymbolBody(mock, 0)
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"uri":                 uri,
		"name_path":           "Foo",
		"content":             "func Foo() { return }",
		"dry_run":             &dryRun,
		"include_doc_comment": true,
	})

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "package main\n\nfunc Foo() { return }\n"
	if string(got) != want {
		t.Errorf("file mismatch\n got: %q\nwant: %q", got, want)
	}
}

// hasUnifiedDiff reports whether s carries a unified-diff block (the ---/+++
// headers plus at least one +/- line), the shape unifiedDiff() emits.
func hasUnifiedDiff(s string) bool {
	return strings.Contains(s, "--- a/") && strings.Contains(s, "+++ b/") &&
		(strings.Contains(s, "\n+") || strings.Contains(s, "\n-"))
}

func TestReplaceSymbolBody_DiffInPreviewAndApplied(t *testing.T) {
	src := "package main\n\nfunc Foo() {}\n"
	mkArgs := func(dry bool) json.RawMessage {
		_, uri := writeFixture(t, "main.go", src)
		args, _ := json.Marshal(map[string]any{
			"uri": uri, "name_path": "Foo",
			"content": "func Foo() { return }", "dry_run": &dry,
		})
		return args
	}

	// dry_run preview carries a diff and leaves the file untouched.
	dryPath, dryURI := writeFixture(t, "dry.go", src)
	dryArgs, _ := json.Marshal(map[string]any{
		"uri": dryURI, "name_path": "Foo",
		"content": "func Foo() { return }", "dry_run": true,
	})
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
	out, err := tools.NewReplaceSymbolBody(mock, 0).Execute(context.Background(), dryArgs)
	if err != nil {
		t.Fatalf("dry-run Execute: %v", err)
	}
	if !hasUnifiedDiff(out) {
		t.Errorf("dry-run output missing diff:\n%s", out)
	}
	if got, _ := os.ReadFile(dryPath); string(got) != src {
		t.Errorf("dry-run modified the file: %q", got)
	}

	// applied edit also carries a diff.
	mock = &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
	out, err = tools.NewReplaceSymbolBody(mock, 0).Execute(context.Background(), mkArgs(false))
	if err != nil {
		t.Fatalf("applied Execute: %v", err)
	}
	if !hasUnifiedDiff(out) {
		t.Errorf("applied output missing diff:\n%s", out)
	}
}

func TestReplaceSymbolBody_ShowWriteDiffOffSuppressesDiff(t *testing.T) {
	src := "package main\n\nfunc Foo() {}\n"
	for _, dry := range []bool{true, false} {
		_, uri := writeFixture(t, "main.go", src)
		args, _ := json.Marshal(map[string]any{
			"uri": uri, "name_path": "Foo",
			"content": "func Foo() { return }", "dry_run": &dry,
		})
		mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
		tool := tools.NewReplaceSymbolBody(mock, 0).WithShowWriteDiff(func() bool { return false })
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute (dry=%v): %v", dry, err)
		}
		if hasUnifiedDiff(out) {
			t.Errorf("dry=%v: diff present despite show_write_diff=false:\n%s", dry, out)
		}
	}
}

func TestSafeDeleteSymbol_DiffOnApply(t *testing.T) {
	src := "package main\n\nfunc Foo() {}\n"
	_, uri := writeFixture(t, "main.go", src)
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}, locations: nil}
	dry := false
	args, _ := json.Marshal(map[string]any{"uri": uri, "name_path": "Foo", "dry_run": &dry})
	out, err := tools.NewSafeDeleteSymbol(mock, 0).Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hasUnifiedDiff(out) {
		t.Errorf("safe_delete output missing diff:\n%s", out)
	}
}

func TestInputSchema_IncludesDocCommentFlag(t *testing.T) {
	// Sanity: the three relevant tools must advertise include_doc_comment;
	// insert_after must not.
	type schemaProvider interface{ InputSchema() json.RawMessage }
	cases := []struct {
		name       string
		t          schemaProvider
		shouldHave bool
	}{
		{"insert_before", tools.NewInsertBeforeSymbol(nil, 0), true},
		{"replace", tools.NewReplaceSymbolBody(nil, 0), true},
		{"safe_delete", tools.NewSafeDeleteSymbol(nil, 0), true},
		{"insert_after", tools.NewInsertAfterSymbol(nil, 0), false},
	}
	for _, c := range cases {
		schema := string(c.t.InputSchema())
		has := strings.Contains(schema, "include_doc_comment")
		if has != c.shouldHave {
			t.Errorf("%s: include_doc_comment present=%v, want %v\nschema: %s", c.name, has, c.shouldHave, schema)
		}
	}
}
