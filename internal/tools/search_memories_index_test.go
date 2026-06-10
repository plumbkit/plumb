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

// TestSearchMemories_StaleIndexFallsBackToGrep covers the auto-mode staleness
// branch: a memory written straight to disk (bypassing the index) makes the
// index stale, so search must fall back to grep — which still finds it.
func TestSearchMemories_StaleIndexFallsBackToGrep(t *testing.T) {
	ws := t.TempDir()
	ix, err := memory.OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	tool := NewSearchMemories(func() string { return ws }).WithIndex(func() *memory.Index { return ix })

	// Index one memory so the index is non-empty...
	if err := memory.WriteIndexed(ix, ws, "indexed", "alpha beta", "d"); err != nil {
		t.Fatalf("WriteIndexed: %v", err)
	}
	// ...then add a second memory directly to disk, bypassing the index ⇒ stale.
	if err := memory.Write(ws, "ghost", "gamma-unique-token via file only", "d"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"gamma-unique-token"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "source=memory-fts") {
		t.Errorf("a stale index must fall back to grep, got FTS output:\n%s", out)
	}
	if !strings.Contains(out, "ghost") {
		t.Errorf("grep fallback should find the un-indexed memory, got:\n%s", out)
	}
}

// TestSearchMemories_FtsModeReindexesStale covers the fts-mode staleness branch:
// forcing FTS on a stale index reindexes first, then returns ranked FTS hits.
func TestSearchMemories_FtsModeReindexesStale(t *testing.T) {
	ws := t.TempDir()
	ix, err := memory.OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	tool := NewSearchMemories(func() string { return ws }).WithIndex(func() *memory.Index { return ix })

	// File on disk, never indexed ⇒ the index is stale.
	if err := memory.Write(ws, "fresh-mem", "delta-unique-token notes", "d"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"delta-unique-token","mode":"fts"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "source=memory-fts") {
		t.Errorf("mode=fts should reindex the stale index and use FTS, got:\n%s", out)
	}
	if !strings.Contains(out, "fresh-mem") {
		t.Errorf("should find the memory after reindex, got:\n%s", out)
	}
}

// TestSearchMemories_AutoZeroFtsHitFallsToGrep is the substring-regression fix:
// on a FRESH index, a substring FTS can't tokenise ("essio" inside
// "UserSession") must still be found via the grep fallback.
func TestSearchMemories_AutoZeroFtsHitFallsToGrep(t *testing.T) {
	ws := t.TempDir()
	ix, err := memory.OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	tool := NewSearchMemories(func() string { return ws }).WithIndex(func() *memory.Index { return ix })
	if err := memory.WriteIndexed(ix, ws, "user-session-notes", "the UserSession lifecycle", "d"); err != nil {
		t.Fatalf("WriteIndexed: %v", err)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"essio"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "source=memory-fts") {
		t.Errorf("auto mode with zero FTS hits should fall back to grep, got FTS:\n%s", out)
	}
	if !strings.Contains(out, "user-session-notes") {
		t.Errorf("grep fallback should find the substring, got:\n%s", out)
	}
}

// TestSearchMemories_CaseSensitiveForcesGrep: FTS5 is case-insensitive, so an
// explicit case_sensitive request must take the grep path.
func TestSearchMemories_CaseSensitiveForcesGrep(t *testing.T) {
	ws := t.TempDir()
	ix, err := memory.OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	tool := NewSearchMemories(func() string { return ws }).WithIndex(func() *memory.Index { return ix })
	if err := memory.WriteIndexed(ix, ws, "notes", "UserSession lifecycle", "d"); err != nil {
		t.Fatalf("WriteIndexed: %v", err)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"UserSession","case_sensitive":true}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "source=memory-fts") {
		t.Errorf("case_sensitive must force grep, got FTS:\n%s", out)
	}
}

// TestSearchMemories_FtsModeKeepsEmptyResult: an explicit mode=fts query keeps
// the empty FTS result (no implicit grep fallback).
func TestSearchMemories_FtsModeKeepsEmptyResult(t *testing.T) {
	ws := t.TempDir()
	ix, err := memory.OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	tool := NewSearchMemories(func() string { return ws }).WithIndex(func() *memory.Index { return ix })
	if err := memory.WriteIndexed(ix, ws, "notes", "UserSession lifecycle", "d"); err != nil {
		t.Fatalf("WriteIndexed: %v", err)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"essio","mode":"fts"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "source=memory-fts") || !strings.Contains(out, "No memory matches") {
		t.Errorf("mode=fts must keep the empty FTS result, got:\n%s", out)
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
