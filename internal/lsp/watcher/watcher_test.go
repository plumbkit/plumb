package watcher_test

import (
	"encoding/json"
	"testing"

	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/lsp/watcher"
)

func TestFilter_NoPatterns_AllowsAll(t *testing.T) {
	var f watcher.Filter
	events := []protocol.FileEvent{
		{URI: "file:///p/main.go", Type: protocol.FileChanged},
		{URI: "file:///p/go.mod", Type: protocol.FileChanged},
	}
	got := f.FilterEvents(events)
	if len(got) != len(events) {
		t.Fatalf("expected all %d events, got %d", len(events), len(got))
	}
}

func TestFilter_Register_FiltersUnmatched(t *testing.T) {
	var f watcher.Filter
	raw, _ := json.Marshal(map[string]any{
		"registrations": []any{
			map[string]any{
				"id":     "1",
				"method": "workspace/didChangeWatchedFiles",
				"registerOptions": map[string]any{
					"watchers": []any{
						map[string]any{"globPattern": "**/*.go"},
						map[string]any{"globPattern": "**/go.mod"},
					},
				},
			},
		},
	})
	f.Register(raw)

	events := []protocol.FileEvent{
		{URI: "file:///p/main.go", Type: protocol.FileChanged},
		{URI: "file:///p/go.mod", Type: protocol.FileChanged},
		{URI: "file:///p/README.md", Type: protocol.FileChanged},
	}
	got := f.FilterEvents(events)
	if len(got) != 2 {
		t.Fatalf("expected 2 events (go + mod), got %d: %v", len(got), got)
	}
	for _, ev := range got {
		if ev.URI == "file:///p/README.md" {
			t.Error("README.md should have been filtered out")
		}
	}
}

func TestFilter_Unregister_RemovesPatterns(t *testing.T) {
	var f watcher.Filter
	reg, _ := json.Marshal(map[string]any{
		"registrations": []any{
			map[string]any{
				"id":     "watch-go",
				"method": "workspace/didChangeWatchedFiles",
				"registerOptions": map[string]any{
					"watchers": []any{
						map[string]any{"globPattern": "**/*.go"},
					},
				},
			},
		},
	})
	f.Register(reg)

	unreg, _ := json.Marshal(map[string]any{
		"unregistrations": []any{
			map[string]any{"id": "watch-go"},
		},
	})
	f.Unregister(unreg)

	events := []protocol.FileEvent{
		{URI: "file:///p/main.go", Type: protocol.FileChanged},
		{URI: "file:///p/README.md", Type: protocol.FileChanged},
	}
	// After unregister all patterns removed → filter passes everything.
	got := f.FilterEvents(events)
	if len(got) != 2 {
		t.Fatalf("expected all events after unregister, got %d", len(got))
	}
}

func TestFilter_Register_IgnoresNonWatchedFiles(t *testing.T) {
	var f watcher.Filter
	// Registration for a different method should not add any watcher patterns.
	raw, _ := json.Marshal(map[string]any{
		"registrations": []any{
			map[string]any{
				"id":              "1",
				"method":          "textDocument/didSave",
				"registerOptions": map[string]any{},
			},
		},
	})
	f.Register(raw)

	events := []protocol.FileEvent{
		{URI: "file:///p/main.go", Type: protocol.FileChanged},
	}
	// No watcher patterns registered → allow all.
	got := f.FilterEvents(events)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
}

func TestFilter_GlobPatterns(t *testing.T) {
	cases := []struct {
		pattern string
		uri     string
		want    bool
	}{
		{"**/*.go", "file:///project/internal/foo.go", true},
		{"**/*.go", "file:///project/main.go", true},
		{"**/*.go", "file:///project/main.py", false},
		{"**/go.mod", "file:///project/go.mod", true},
		{"**/go.mod", "file:///project/go.sum", false},
		{"**/*.py", "file:///project/src/app.py", true},
		{"**/*.java", "file:///project/src/Main.java", true},
		{"**/*.java", "file:///project/src/Main.go", false},
	}
	for _, tc := range cases {
		var f watcher.Filter
		raw, _ := json.Marshal(map[string]any{
			"registrations": []any{
				map[string]any{
					"id":     "1",
					"method": "workspace/didChangeWatchedFiles",
					"registerOptions": map[string]any{
						"watchers": []any{map[string]any{"globPattern": tc.pattern}},
					},
				},
			},
		})
		f.Register(raw)
		events := []protocol.FileEvent{{URI: tc.uri, Type: protocol.FileChanged}}
		got := f.FilterEvents(events)
		matched := len(got) == 1
		if matched != tc.want {
			t.Errorf("pattern=%q uri=%q: got matched=%v, want %v", tc.pattern, tc.uri, matched, tc.want)
		}
	}
}
