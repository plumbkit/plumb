package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// TestReplaceSymbolBody_NotifiesLSPAndInvalidatesCache is the RC1 regression:
// a successful semantic edit must tell the language server the file changed on
// disk (workspace/didChangeWatchedFiles) and evict the symbol cache for the
// file — the same post-write housekeeping edit_file/write_file perform. Without
// it the server keeps stale content and the next semantic edit fails
// "position out of range". A dry-run must do neither.
func TestReplaceSymbolBody_NotifiesLSPAndInvalidatesCache(t *testing.T) {
	src := "package main\n\nfunc Foo() {}\n"

	t.Run("apply notifies and invalidates", func(t *testing.T) {
		path, uri := writeFixture(t, "main.go", src)
		mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
		c := cache.New(0)
		defer c.Close()
		cacheKey := uri + ":documentSymbol"
		c.Set(cacheKey, "stale", 0)

		dry := false
		args, _ := json.Marshal(map[string]any{
			"uri": uri, "name_path": "Foo",
			"content": "func Foo() { return }", "dry_run": &dry,
		})
		tool := tools.NewReplaceSymbolBody(mock, 0).WithCache(c)
		if _, err := tool.Execute(context.Background(), args); err != nil {
			t.Fatalf("Execute: %v", err)
		}

		if len(mock.watchedEvents) != 1 {
			t.Fatalf("expected 1 DidChangeWatchedFiles event, got %d", len(mock.watchedEvents))
		}
		if mock.watchedEvents[0].Type != protocol.FileChanged {
			t.Errorf("event type = %v, want FileChanged", mock.watchedEvents[0].Type)
		}
		if mock.watchedEvents[0].URI != protocol.FileURI(path) {
			t.Errorf("event URI = %q, want %q", mock.watchedEvents[0].URI, protocol.FileURI(path))
		}
		if _, ok := c.Get(cacheKey); ok {
			t.Errorf("cache entry %q survived the edit — not invalidated", cacheKey)
		}
	})

	t.Run("dry-run notifies nothing and keeps cache", func(t *testing.T) {
		_, uri := writeFixture(t, "main.go", src)
		mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
		c := cache.New(0)
		defer c.Close()
		cacheKey := uri + ":documentSymbol"
		c.Set(cacheKey, "fresh", 0)

		dry := true
		args, _ := json.Marshal(map[string]any{
			"uri": uri, "name_path": "Foo",
			"content": "func Foo() { return }", "dry_run": &dry,
		})
		tool := tools.NewReplaceSymbolBody(mock, 0).WithCache(c)
		if _, err := tool.Execute(context.Background(), args); err != nil {
			t.Fatalf("Execute: %v", err)
		}

		if len(mock.watchedEvents) != 0 {
			t.Errorf("dry-run sent %d notifications, want 0", len(mock.watchedEvents))
		}
		if _, ok := c.Get(cacheKey); !ok {
			t.Errorf("dry-run evicted the cache entry")
		}
	})
}

// TestRenameSymbol_NotifiesEachModifiedFile is the RC1 regression for the
// multi-file rename path: every rewritten file must be notified and its cache
// entries evicted, or a follow-up query against any of them resolves stale
// positions.
func TestRenameSymbol_NotifiesEachModifiedFile(t *testing.T) {
	aPath, aURI := writeFixture(t, "a.go", "package main\n\nvar Foo = 1\n")
	bPath, bURI := writeFixture(t, "b.go", "package main\n\nvar _ = Foo\n")

	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			aURI: {{Range: protocol.Range{
				Start: protocol.Position{Line: 2, Character: 4},
				End:   protocol.Position{Line: 2, Character: 7},
			}, NewText: "Bar"}},
			bURI: {{Range: protocol.Range{
				Start: protocol.Position{Line: 2, Character: 8},
				End:   protocol.Position{Line: 2, Character: 11},
			}, NewText: "Bar"}},
		},
	}
	mock := &mockLSP{renameResult: we}
	c := cache.New(0)
	defer c.Close()
	c.Set(aURI+":documentSymbol", "stale", 0)
	c.Set(bURI+":documentSymbol", "stale", 0)

	dry := false
	args, _ := json.Marshal(map[string]any{
		"uri": aURI, "line": 2, "character": 4, "new_name": "Bar", "dry_run": &dry,
	})
	tool := tools.NewRenameSymbol(mock, 0).WithCache(c)
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(mock.watchedEvents) != 2 {
		t.Fatalf("expected 2 notifications (one per file), got %d", len(mock.watchedEvents))
	}
	notified := map[string]bool{}
	for _, e := range mock.watchedEvents {
		if e.Type != protocol.FileChanged {
			t.Errorf("event type = %v, want FileChanged", e.Type)
		}
		notified[e.URI] = true
	}
	for _, want := range []string{protocol.FileURI(aPath), protocol.FileURI(bPath)} {
		if !notified[want] {
			t.Errorf("file %q was not notified", want)
		}
	}
	if _, ok := c.Get(aURI + ":documentSymbol"); ok {
		t.Errorf("a.go cache entry survived the rename")
	}
	if _, ok := c.Get(bURI + ":documentSymbol"); ok {
		t.Errorf("b.go cache entry survived the rename")
	}
}

func TestReplaceSymbolBody_WithWriteDepsRecordsFullWriteBookkeeping(t *testing.T) {
	path, uri := writeFixture(t, "main.go", "package main\n\nfunc Foo() {}\n")
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
	writes := tools.NewWriteTracker()
	undo := tools.NewUndoStore()
	var topologyNotified []string
	var postWriteNotified []string
	deps := tools.WriteDeps{
		Client: mock,
		Writes: writes,
		Undo:   undo,
		PostWriteNotifyFn: func(_ context.Context, p string) error {
			postWriteNotified = append(postWriteNotified, p)
			return nil
		},
		TopologyNotify: func(p string) { topologyNotified = append(topologyNotified, p) },
		ShowWriteDiff:  true,
	}

	dry := false
	args, _ := json.Marshal(map[string]any{
		"uri": uri, "name_path": "Foo", "content": "func Foo() { return }", "dry_run": &dry,
	})
	tool := tools.NewReplaceSymbolBody(mock, 0).WithWriteDeps(deps)
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !writes.Wrote(path) {
		t.Fatalf("semantic edit did not record the path in WriteTracker")
	}
	if len(postWriteNotified) != 1 || postWriteNotified[0] != path {
		t.Fatalf("PostWriteNotifyFn calls = %v, want [%s]", postWriteNotified, path)
	}
	if len(topologyNotified) != 1 || topologyNotified[0] != path {
		t.Fatalf("TopologyNotify calls = %v, want [%s]", topologyNotified, path)
	}
	if _, ok := undo.Peek(path); !ok {
		t.Fatal("semantic edit did not record an undo snapshot")
	}

	undoArgs, _ := json.Marshal(map[string]any{"file_path": path})
	if _, err := tools.NewUndoEdit(deps).Execute(context.Background(), undoArgs); err != nil {
		t.Fatalf("undo_edit after semantic edit: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "func Foo() {}") || strings.Contains(string(got), "return") {
		t.Fatalf("undo did not restore the pre-edit body: %s", got)
	}
}
