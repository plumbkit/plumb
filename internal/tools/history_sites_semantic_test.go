package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestApplyWorkspaceEditRecordsEachFile(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "f1.go")
	p2 := filepath.Join(dir, "f2.go")
	if err := os.WriteFile(p1, []byte("package p\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("package p\nfunc Bar() { Foo() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			protocol.FileURI(p1): {{
				Range:   protocol.Range{Start: protocol.Position{Line: 1, Character: 5}, End: protocol.Position{Line: 1, Character: 8}},
				NewText: "Renamed",
			}},
			protocol.FileURI(p2): {{
				Range:   protocol.Range{Start: protocol.Position{Line: 1, Character: 13}, End: protocol.Position{Line: 1, Character: 16}},
				NewText: "Renamed",
			}},
		},
	}

	var f fakeHistory
	sink := historySink(func(c history.Change) { f.record(context.Background(), c) })
	_, _, err := applyWorkspaceEditDetailed(we, nil, sink, "rename_symbol")
	if err != nil {
		t.Fatal(err)
	}

	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes, got %d: %+v", len(cs), cs)
	}
	for _, c := range cs {
		if c.Op != history.OpUpdate || c.Tool != "rename_symbol" || !c.Before.Exists || !c.After.Exists {
			t.Errorf("unexpected change: %+v", c)
		}
	}
}

func TestWorkspaceEditRollbackRecordsReverts(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p1, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zdir := filepath.Join(dir, "z")
	if err := os.Mkdir(zdir, 0o755); err != nil {
		t.Fatal(err)
	}
	p2 := filepath.Join(zdir, "z.go")
	if err := os.WriteFile(p2, []byte("original z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(zdir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(zdir, 0o755) })

	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			protocol.FileURI(p1): {{
				Range:   protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 8}},
				NewText: "modified",
			}},
			protocol.FileURI(p2): {{
				Range:   protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 10}},
				NewText: "modified z",
			}},
		},
	}

	var f fakeHistory
	sink := historySink(func(c history.Change) { f.record(context.Background(), c) })
	_, _, err := applyWorkspaceEditDetailed(we, nil, sink, "rename_symbol")
	if err == nil {
		t.Fatal("expected applyWorkspaceEditDetailed to fail")
	}

	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes (update then revert), got %d: %+v", len(cs), cs)
	}
	if cs[0].Op != history.OpUpdate || cs[0].Path != p1 {
		t.Errorf("expected first change to be OpUpdate on p1, got %+v", cs[0])
	}
	if cs[1].Op != history.OpRevert || cs[1].Path != p1 || !cs[1].RevertsOwnCall || cs[1].Reason != "edit_rollback" {
		t.Errorf("expected second change to be OpRevert edit_rollback on p1, got %+v", cs[1])
	}
	// The revert is filed under the tool that rolled back, so `plumb history
	// --tool rename_symbol` lists it beside the write it undid.
	if cs[1].Tool != "rename_symbol" {
		t.Errorf("revert Tool = %q, want the calling tool rename_symbol", cs[1].Tool)
	}
}

func TestMoveRollbackRemovingACreatedFileRecordsRevert(t *testing.T) {
	dir := t.TempDir()
	created := filepath.Join(dir, "created.go")
	plans := []movePlan{
		{path: created, after: []byte("package x\n"), mode: 0o644, existedBefore: false},
		{path: blockerChild(t, dir), after: []byte("new\n"), mode: 0o644, existedBefore: false},
	}

	var f fakeHistory
	sink := historySink(func(c history.Change) { f.record(context.Background(), c) })
	_, err := applyMovePlans(plans, nil, sink)
	if err == nil {
		t.Fatal("expected applyMovePlans to fail")
	}

	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes (create then revert), got %d: %+v", len(cs), cs)
	}
	if cs[0].Op != history.OpCreate || cs[0].Path != created {
		t.Errorf("expected OpCreate on created, got %+v", cs[0])
	}
	if cs[1].Op != history.OpRevert || cs[1].Path != created || cs[1].After.Exists || !cs[1].RevertsOwnCall || cs[1].Reason != "move_rollback" {
		t.Errorf("expected OpRevert move_rollback removing file on created, got %+v", cs[1])
	}
}

func TestTransactionRecordsUpdates(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "f1.txt")
	p2 := filepath.Join(dir, "f2.txt")
	if err := os.WriteFile(p1, []byte("hello A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("hello B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record, WorkspaceFn: func(_ context.Context) string { return dir }}
	txTool := NewTransactionApply(deps)

	_, err := txTool.Execute(context.Background(), mustJSON(map[string]any{
		"operations": []map[string]any{
			{"file_path": p1, "edits": []map[string]string{{"old_string": "hello A", "new_string": "hi A"}}},
			{"file_path": p2, "edits": []map[string]string{{"old_string": "hello B", "new_string": "hi B"}}},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}

	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes, got %d: %+v", len(cs), cs)
	}
	for _, c := range cs {
		if c.Op != history.OpUpdate || c.Tool != "transaction_apply" || !c.Before.Exists || !c.After.Exists {
			t.Errorf("unexpected change: %+v", c)
		}
	}
}

func TestTransactionPartialFailureRecordsRevertsBeforeTheAntecedentIsWritten(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "f1.txt")
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p2 := filepath.Join(sub, "f2.txt")
	if err := os.WriteFile(p1, []byte("hello A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("hello B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	var f fakeHistory
	deps := WriteDeps{
		HistoryFn:   f.record,
		WorkspaceFn: func(_ context.Context) string { return dir },
	}
	txTool := NewTransactionApply(deps)

	_, err := txTool.Execute(context.Background(), mustJSON(map[string]any{
		"operations": []map[string]any{
			{"file_path": p1, "edits": []map[string]string{{"old_string": "hello A", "new_string": "hi A"}}},
			{"file_path": p2, "edits": []map[string]string{{"old_string": "hello B", "new_string": "hi B"}}},
		},
	}))
	if err == nil {
		t.Fatal("expected transaction_apply to fail on unwritable sub directory")
	}

	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes (update then revert), got %d: %+v", len(cs), cs)
	}
	if cs[0].Op != history.OpUpdate || cs[0].Path != p1 {
		t.Errorf("expected update on p1, got %+v", cs[0])
	}
	if cs[1].Op != history.OpRevert || cs[1].Path != p1 || !cs[1].RevertsOwnCall || cs[1].Reason != "tx_rollback" {
		t.Errorf("expected revert tx_rollback on p1, got %+v", cs[1])
	}
}

func TestFailOnNewErrorsRevertRecordsRevert(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "revert.txt")
	if err := os.WriteFile(p, []byte("wrote\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	req := rollbackRequest{
		tool:          "write_file",
		path:          p,
		uri:           "file://" + p,
		before:        "before\n",
		existedBefore: true,
		wrote:         "wrote\n",
	}

	holds, err := deps.revertWrite(context.Background(), req)
	if err != nil {
		t.Fatalf("revertWrite failed: %v (holds: %s)", err, holds)
	}

	c := f.only(t)
	if c.Op != history.OpRevert || c.Reason != "new_errors" || !c.RevertsOwnCall || c.Tool != "write_file" {
		t.Fatalf("unexpected change: %+v", c)
	}
	if string(c.Before.Content) != "wrote\n" || string(c.After.Content) != "before\n" {
		t.Fatalf("content mismatch: %+v", c)
	}
}

func TestSymbolEditRecordsUpdate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sym.go")
	if err := os.WriteFile(p, []byte("func Old() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	resolver := func(context.Context) (protocol.TextEdit, *protocol.DocumentSymbol, string, error) {
		return protocol.TextEdit{
			Range:   protocol.Range{Start: protocol.Position{Line: 0, Character: 5}, End: protocol.Position{Line: 0, Character: 8}},
			NewText: "New",
		}, &protocol.DocumentSymbol{Name: "Old"}, "", nil
	}

	_, err := applySingleEdit(context.Background(), nil, nil, &deps, protocol.FileURI(p), false, true, "replaced", "replace_symbol_body", true, resolver)
	if err != nil {
		t.Fatal(err)
	}

	c := f.only(t)
	if c.Op != history.OpUpdate || c.Tool != "replace_symbol_body" || !c.Before.Exists || !c.After.Exists {
		t.Fatalf("unexpected change: %+v", c)
	}
	if string(c.Before.Content) != "func Old() {}\n" || string(c.After.Content) != "func New() {}\n" {
		t.Fatalf("content mismatch: %+v", c)
	}
}
