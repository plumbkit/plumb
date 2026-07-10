package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestRenameSymbol_StaleIndexError(t *testing.T) {
	// Write a small file to a temp dir.
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package foo\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The LSP returns a WorkspaceEdit whose edit positions are past the end of
	// the file, simulating a stale position index after an in-session edit.
	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file://" + path: {
				{
					Range: protocol.Range{
						Start: protocol.Position{Line: 999, Character: 0},
						End:   protocol.Position{Line: 999, Character: 3},
					},
					NewText: "Bar",
				},
			},
		},
	}

	mock := &mockLSP{renameResult: we}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri":       "file://" + path,
		"line":      2,
		"character": 5,
		"new_name":  "Bar",
		"dry_run":   false,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "position index is stale") {
		t.Errorf("expected stale-index hint in error; got: %s", msg)
	}
	if !strings.Contains(msg, "find_replace") {
		t.Errorf("expected find_replace fallback suggestion in error; got: %s", msg)
	}
}

// renameEditFor builds a WorkspaceEdit replacing the "Foo" identifier on line 2
// (char 5–8) of the standard test source with newText.
func renameEditFor(path, newText string) *protocol.WorkspaceEdit {
	return &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file://" + path: {
				{
					Range: protocol.Range{
						Start: protocol.Position{Line: 2, Character: 5},
						End:   protocol.Position{Line: 2, Character: 8},
					},
					NewText: newText,
				},
			},
		},
	}
}

func TestRenameSymbol_AppliedShowsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{renameResult: renameEditFor(path, "Bar")}
	tool := tools.NewRenameSymbol(mock, 0) // showDiff unwired → defaults on

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 5, "new_name": "Bar", "dry_run": false,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(out, "--- a/"+path) || !strings.Contains(out, "-func Foo() {}") || !strings.Contains(out, "+func Bar() {}") {
		t.Errorf("expected applied unified diff; got: %s", out)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "func Bar() {}") {
		t.Errorf("rename not applied: %s", got)
	}
}

func TestRenameSymbol_DryRunShowsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	src := "package p\n\nfunc Foo() {}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{renameResult: renameEditFor(path, "Bar")}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 5, "new_name": "Bar", // dry_run defaults true
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "+func Bar() {}") {
		t.Errorf("expected dry-run preview diff; got: %s", out)
	}
	if got, _ := os.ReadFile(path); string(got) != src {
		t.Errorf("dry run must not modify the file; got: %s", got)
	}
}

func TestRenameSymbol_ShowDiffGateOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{renameResult: renameEditFor(path, "Bar")}
	tool := tools.NewRenameSymbol(mock, 0).WithShowWriteDiff(func() bool { return false })

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 5, "new_name": "Bar", "dry_run": false,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if strings.Contains(out, "--- a/") || strings.Contains(out, "@@") {
		t.Errorf("show_write_diff=false must suppress the diff; got: %s", out)
	}
	if !strings.Contains(out, "changed") {
		t.Errorf("expected the file summary even with the diff off; got: %s", out)
	}
}

func TestRenameSymbol_MultiFileDiffTruncation(t *testing.T) {
	dir := t.TempDir()
	changes := map[string][]protocol.TextEdit{}
	const nFiles = 25 // maxRenameDiffFiles (20) + 5 beyond the cap
	for i := 0; i < nFiles; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%02d.go", i))
		if err := os.WriteFile(p, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		changes["file://"+p] = []protocol.TextEdit{{
			Range:   protocol.Range{Start: protocol.Position{Line: 2, Character: 5}, End: protocol.Position{Line: 2, Character: 8}},
			NewText: "Bar",
		}}
	}
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{Changes: changes}}
	tool := tools.NewRenameSymbol(mock, 0)
	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + filepath.Join(dir, "f00.go"), "line": 2, "character": 5, "new_name": "Bar", "dry_run": false,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(out, "and 5 more file(s)") {
		t.Errorf("expected truncation summary for files beyond the cap; got tail: %s", out[max(0, len(out)-300):])
	}
}

func TestRenameSymbol_EmptyEditSet(t *testing.T) {
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{}}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri":       "file:///any.go",
		"line":      0,
		"character": 0,
		"new_name":  "NewName",
	})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "empty edit set") {
		t.Errorf("expected empty-edit-set message; got: %s", out)
	}
}

func TestRenameSymbol_LSPError_ActionableHint(t *testing.T) {
	mock := &mockLSP{err: errors.New("server exploded")}
	tool := tools.NewRenameSymbol(mock, 0)
	args, _ := json.Marshal(map[string]any{
		"uri": "file:///nope.go", "line": 0, "character": 0, "new_name": "X",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not compute this rename") {
		t.Errorf("missing actionable hint: %s", msg)
	}
	if !strings.Contains(msg, "find_references") {
		t.Errorf("missing find_references fallback: %s", msg)
	}
	// structural_fallback must not be suggested when the fallback is not wired.
	if strings.Contains(msg, "structural_fallback") {
		t.Errorf("should not suggest structural_fallback when unwired: %s", msg)
	}
}

func TestRenameSymbol_LSPError_SuggestsStructuralWhenWired(t *testing.T) {
	mock := &mockLSP{err: errors.New("server exploded")}
	tool := tools.NewRenameSymbol(mock, 0).WithStructuralFallback(tools.WriteDeps{})
	args, _ := json.Marshal(map[string]any{
		"uri": "file:///nope.go", "line": 0, "character": 0, "new_name": "X",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "structural_fallback:true") {
		t.Errorf("expected structural_fallback suggestion: %s", err.Error())
	}
}

func TestRenameSymbol_StructuralFallback_DryRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	src := "package p\n\nfunc Foo() {}\nvar x = Foo\nvar FooBar = 1\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{err: errors.New("no index")}
	tool := tools.NewRenameSymbol(mock, 0).
		WithWorkspace(func() string { return dir }).
		WithStructuralFallback(tools.WriteDeps{})
	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 5, "new_name": "Renamed",
		"structural_fallback": true, // dry_run defaults true
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(out, "STRUCTURAL FALLBACK") || !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected fallback dry-run banner: %s", out)
	}
	if b, _ := os.ReadFile(path); string(b) != src {
		t.Errorf("dry run must not modify the file; got: %s", b)
	}
}

func TestRenameSymbol_StructuralFallback_Applies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	src := "package p\n\nfunc Foo() {}\nvar x = Foo\nvar FooBar = 1\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{err: errors.New("no index")}
	tool := tools.NewRenameSymbol(mock, 0).
		WithWorkspace(func() string { return dir }).
		WithStructuralFallback(tools.WriteDeps{})
	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 5, "new_name": "Renamed",
		"structural_fallback": true, "dry_run": false,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	got, _ := os.ReadFile(path)
	gs := string(got)
	if !strings.Contains(gs, "func Renamed()") || !strings.Contains(gs, "var x = Renamed") {
		t.Errorf("expected Foo->Renamed: %s", gs)
	}
	// Word boundary: FooBar shares a prefix but must NOT be renamed.
	if !strings.Contains(gs, "var FooBar = 1") {
		t.Errorf("word-boundary violated, FooBar changed: %s", gs)
	}
}

func TestRenameSymbol_StructuralFallback_OnEmptyEditSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\nvar Foo = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{}} // empty edit set
	tool := tools.NewRenameSymbol(mock, 0).
		WithWorkspace(func() string { return dir }).
		WithStructuralFallback(tools.WriteDeps{})
	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 1, "character": 4, "new_name": "Bar",
		"structural_fallback": true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(out, "STRUCTURAL FALLBACK") {
		t.Errorf("expected fallback on empty edit set: %s", out)
	}
}

func TestRenameSymbol_ByNameUsesSelectionRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{{
			Name:           "Foo",
			Range:          protocol.Range{Start: protocol.Position{Line: 2, Character: 0}, End: protocol.Position{Line: 2, Character: 13}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}, End: protocol.Position{Line: 2, Character: 8}},
		}},
		renameResult: renameEditFor(path, "Bar"),
	}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "symbol_name": "Foo", "new_name": "Bar", "dry_run": false,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := (protocol.Position{Line: 2, Character: 5})
	if mock.lastRenamePos != want {
		t.Fatalf("Rename position = %+v, want SelectionRange.Start %+v", mock.lastRenamePos, want)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "func Bar() {}") {
		t.Fatalf("rename did not apply: %s", got)
	}
}

func TestRenameSymbol_RawPositionMissSnapsAndRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{{
			Name:           "Foo",
			Range:          protocol.Range{Start: protocol.Position{Line: 2, Character: 0}, End: protocol.Position{Line: 2, Character: 13}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}, End: protocol.Position{Line: 2, Character: 8}},
		}},
		renameResult: renameEditFor(path, "Bar"),
		renameErrs:   []error{errors.New("no identifier found")},
	}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 0, "new_name": "Bar", "dry_run": false,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := (protocol.Position{Line: 2, Character: 5})
	if mock.lastRenamePos != want {
		t.Fatalf("retry Rename position = %+v, want snapped SelectionRange.Start %+v", mock.lastRenamePos, want)
	}
	if !strings.Contains(out, "answered for the enclosing symbol") {
		t.Fatalf("expected snap notice in output, got: %s", out)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "func Bar() {}") {
		t.Fatalf("rename did not apply: %s", got)
	}
}

func TestRenameSymbol_AmbiguousNameRefusesStructuralFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	src := "package p\n\nfunc Foo() {}\nfunc Foo2() {}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	sym := func(line uint32) protocol.DocumentSymbol {
		return protocol.DocumentSymbol{
			Name:           "Foo",
			Range:          protocol.Range{Start: protocol.Position{Line: line, Character: 0}, End: protocol.Position{Line: line, Character: 13}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: line, Character: 5}, End: protocol.Position{Line: line, Character: 8}},
		}
	}
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{sym(2), sym(3)}}
	tool := tools.NewRenameSymbol(mock, 0).
		WithWorkspace(func() string { return dir }).
		WithStructuralFallback(tools.WriteDeps{})

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "symbol_name": "Foo", "new_name": "Bar",
		"structural_fallback": true, "dry_run": false,
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected an ambiguity error, got success — the structural fallback must not run for an ambiguous symbol_name")
	}
	if !strings.Contains(err.Error(), "disambiguate") {
		t.Errorf("expected disambiguation guidance, got: %v", err)
	}
	if strings.Contains(err.Error(), "could not compute this rename") {
		t.Errorf("plumb-side ambiguity must not carry the LSP-failure hint: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != src {
		t.Errorf("file must be untouched after a refused ambiguous rename; got: %s", b)
	}
}

func TestRenameSymbol_MissingArgsNoLSPHint(t *testing.T) {
	tool := tools.NewRenameSymbol(&mockLSP{}, 0)
	args, _ := json.Marshal(map[string]any{
		"uri": "file:///x.go", "new_name": "Bar",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if !strings.Contains(err.Error(), "either symbol_name or both line and character are required") {
		t.Errorf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "could not compute this rename") {
		t.Errorf("argument validation must not carry the LSP-failure hint: %v", err)
	}
}

// A wide rename must notify the language server, the cache, and the topology
// index about EVERY file it changed, while paying the expensive post-write
// report — the blocking diagnostics wait plus a lint run — only for a bounded
// prefix. Before this cap a 50-file rename serialised 50 diagnostics waits and
// 50 analyser invocations into one response.
func TestRenameSymbol_WideRenameCapsPostWriteReporting(t *testing.T) {
	const files = 8
	dir := t.TempDir()
	changes := map[string][]protocol.TextEdit{}
	for i := range files {
		path := filepath.Join(dir, fmt.Sprintf("f%d.go", i))
		if err := os.WriteFile(path, []byte("package p\n\nvar Foo = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		changes["file://"+path] = []protocol.TextEdit{{
			Range:   protocol.Range{Start: protocol.Position{Line: 2, Character: 4}, End: protocol.Position{Line: 2, Character: 7}},
			NewText: "Bar",
		}}
	}

	var quality, indexed int
	deps := tools.WriteDeps{
		QualityReport:  func(context.Context, string) string { quality++; return "" },
		TopologyNotify: func(string) { indexed++ },
	}
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{Changes: changes}}
	tool := tools.NewRenameSymbol(mock, 0).WithWriteDeps(deps)

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + filepath.Join(dir, "f0.go"), "line": 2, "character": 4,
		"new_name": "Bar", "dry_run": false,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if indexed != files {
		t.Errorf("every modified file must be re-indexed: got %d topology notifications, want %d", indexed, files)
	}
	if quality != 5 {
		t.Errorf("quality analysis must be capped: got %d runs over %d files, want 5", quality, files)
	}
	if !strings.Contains(out, "reported for the first 5 of 8 modified file(s)") {
		t.Errorf("expected the capped-reporting notice, got:\n%s", out)
	}
	for i := range files {
		b, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%d.go", i)))
		if !strings.Contains(string(b), "var Bar = 1") {
			t.Fatalf("f%d.go was not renamed: %s", i, b)
		}
	}
}

// A rename narrow enough to report on every file must not carry the cap notice.
func TestRenameSymbol_NarrowRenameReportsEveryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nvar Foo = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var quality int
	deps := tools.WriteDeps{QualityReport: func(context.Context, string) string { quality++; return "" }}
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
		"file://" + path: {{
			Range:   protocol.Range{Start: protocol.Position{Line: 2, Character: 4}, End: protocol.Position{Line: 2, Character: 7}},
			NewText: "Bar",
		}},
	}}}
	tool := tools.NewRenameSymbol(mock, 0).WithWriteDeps(deps)

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 4, "new_name": "Bar", "dry_run": false,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if quality != 1 {
		t.Errorf("quality runs = %d, want 1", quality)
	}
	if strings.Contains(out, "reported for the first") {
		t.Errorf("a single-file rename must not carry the cap notice, got:\n%s", out)
	}
}

// The snap-and-retry is single-shot: if the retried position ALSO misses, the
// tool must surface the error rather than snap again. Seeding two miss errors
// and counting Rename calls pins that contract — a retry loop would keep calling
// until the seeded errors ran out and then succeed, hiding the regression.
func TestRenameSymbol_SnapRetryIsSingleShot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{{
			Name:           "Foo",
			Range:          protocol.Range{Start: protocol.Position{Line: 2, Character: 0}, End: protocol.Position{Line: 2, Character: 13}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}, End: protocol.Position{Line: 2, Character: 8}},
		}},
		renameResult: renameEditFor(path, "Bar"),
		renameErrs: []error{
			errors.New("no identifier found"), // the original raw position
			errors.New("no identifier found"), // the snapped retry
		},
	}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 0, "new_name": "Bar", "dry_run": false,
	})
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("a snapped retry that also misses must surface the error, not snap again")
	}
	if mock.renameCalls != 2 {
		t.Errorf("Rename called %d times, want exactly 2 (the original + one snapped retry)", mock.renameCalls)
	}
	if got, _ := os.ReadFile(path); string(got) != "package p\n\nfunc Foo() {}\n" {
		t.Errorf("a failed rename must not touch the file: %s", got)
	}
}

// A symbol_name caller supplied no coordinates, so a server rejection of the
// position plumb resolved for it must not be explained with a hint about the
// line and character arguments it never passed.
func TestRenameSymbol_ByName_ServerRejectionHintDoesNotMentionCoordinates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{{
			Name:           "Foo",
			Range:          protocol.Range{Start: protocol.Position{Line: 2, Character: 0}, End: protocol.Position{Line: 2, Character: 13}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}, End: protocol.Position{Line: 2, Character: 8}},
		}},
		renameErrs: []error{errors.New("no identifier found")},
	}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "symbol_name": "Foo", "new_name": "Bar", "dry_run": false,
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected the server rejection to surface")
	}
	if strings.Contains(err.Error(), "0-based") {
		t.Errorf("a symbol_name caller must not be pointed at line/character it never passed: %v", err)
	}
	if !strings.Contains(err.Error(), `symbol "Foo"`) || !strings.Contains(err.Error(), "index is stale") {
		t.Errorf("expected a stale-symbol-tree hint naming the symbol, got: %v", err)
	}
	// A by-name rename must not snap: the position already came from the tree.
	if mock.renameCalls != 1 {
		t.Errorf("Rename called %d times, want 1 (no snap on a by-name query)", mock.renameCalls)
	}
}

// A raw-position caller keeps the coordinate hint.
func TestRenameSymbol_ByPosition_ServerRejectionKeepsCoordinateHint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{renameErrs: []error{errors.New("boom")}}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 5, "new_name": "Bar", "dry_run": false,
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected the server error to surface")
	}
	if !strings.Contains(err.Error(), "0-based") {
		t.Errorf("a raw-position caller keeps the coordinate hint, got: %v", err)
	}
}

// A server that names one file in both Changes and DocumentChanges (nothing in
// the protocol forbids it) must still be treated as one target: the apply path
// groups its plans by URI, so a duplicated entry here would shift the file list
// out of step with the plans and cost a reported file its pre-write baseline.
func TestRenameSymbol_DeduplicatesTargetsAcrossEditForms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nvar Foo = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := "file://" + path
	edit := protocol.TextEdit{
		Range:   protocol.Range{Start: protocol.Position{Line: 2, Character: 4}, End: protocol.Position{Line: 2, Character: 7}},
		NewText: "Bar",
	}
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{
		Changes:         map[string][]protocol.TextEdit{uri: {edit}},
		DocumentChanges: []protocol.TextDocumentEdit{{TextDocument: protocol.VersionedTextDocumentIdentifier{URI: uri}}},
	}}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri": uri, "line": 2, "character": 4, "new_name": "Bar", "dry_run": true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "across 1 file(s)") {
		t.Errorf("one file named twice must be counted once, got:\n%s", out)
	}
}
