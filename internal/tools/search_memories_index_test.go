package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/memory"
)

func TestSearchMemories_FTSAndGrepFallback(t *testing.T) {
	ws := t.TempDir()
	ix, err := memory.OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()

	tool := NewSearchMemories(func() string { return ws }).
		WithIndex(func() *memory.Index { return ix })

	if err := memory.WriteIndexed(ix, ws, "auth-notes",
		"# Auth\n\nthe UserSession token lifecycle", "auth notes"); err != nil {
		t.Fatalf("WriteIndexed: %v", err)
	}

	// FTS path: a CamelCase-ish query resolves via the index, ranked.
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"user session"}`))
	if err != nil {
		t.Fatalf("Execute fts: %v", err)
	}
	if !strings.Contains(out, "source=memory-fts") || !strings.Contains(out, "auth-notes") {
		t.Errorf("expected ranked FTS hit, got:\n%s", out)
	}

	// mode=grep forces the deterministic grep path (no FTS header).
	out2, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"lifecycle","mode":"grep"}`))
	if err != nil {
		t.Fatalf("Execute grep: %v", err)
	}
	if strings.Contains(out2, "source=memory-fts") {
		t.Errorf("mode=grep must not use FTS, got:\n%s", out2)
	}
	if !strings.Contains(out2, "auth-notes") {
		t.Errorf("grep should match the line, got:\n%s", out2)
	}
}

func TestSearchMemories_NilIndexUsesGrep(t *testing.T) {
	ws := t.TempDir()
	if err := memory.Write(ws, "n", "zebra-unique-token", "d"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tool := NewSearchMemories(func() string { return ws }) // no index wired
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"zebra-unique-token"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "source=memory-fts") {
		t.Errorf("no index ⇒ grep, got:\n%s", out)
	}
	if !strings.Contains(out, "n:") {
		t.Errorf("grep should find the memory, got:\n%s", out)
	}
}
