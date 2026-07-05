package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/memory"
)

// shareFindingsTestDeps builds ShareFindingsDeps backed by a real temp workspace.
// It uses a nil memory index (the write degrades to a plain markdown memory —
// still redacted, provenance-stamped, path-tagged, and retention-pruned), which
// keeps the tests free of an FTS index while exercising the full pipeline shape.
func shareFindingsTestDeps(t *testing.T, policy CollabPolicy, keep int) (ShareFindingsDeps, string) {
	t.Helper()
	ws := t.TempDir()
	deps := ShareFindingsDeps{
		Workspace:           func() string { return ws },
		SessionName:         func() string { return "test-session" },
		SessionID:           "sess-abcdef01",
		Policy:              func() CollabPolicy { return policy },
		Index:               func() *memory.Index { return nil },
		GeneratedMemoryKeep: func() int { return keep },
	}
	return deps, ws
}

func TestShareFindings_DisabledRefusesCleanly(t *testing.T) {
	deps, ws := shareFindingsTestDeps(t, CollabPolicy{KnowledgeHandoff: false}, 50)
	tool := NewShareFindings(deps)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"summary":"mapped the pool"}`))
	if err != nil {
		t.Fatalf("disabled should not error: %v", err)
	}
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "knowledge_handoff = true") {
		t.Errorf("expected a clear enable hint naming the config key; got %q", out)
	}
	mems, _ := memory.List(ws)
	if len(mems) != 0 {
		t.Errorf("the disabled path must not write a memory; got %d", len(mems))
	}
}

func TestShareFindings_EnabledWritesRedactedProvenancedMemory(t *testing.T) {
	deps, ws := shareFindingsTestDeps(t, CollabPolicy{KnowledgeHandoff: true}, 50)
	tool := NewShareFindings(deps)
	body := `the pool is keyed by (root, language) api_key=SUPERSECRETVALUE123456`
	payload := `{"summary":` + jsonStr(body) + `,"description":"secondaries live to shutdown","paths":["internal/cli/pool*.go"]}`
	out, err := tool.Execute(context.Background(), json.RawMessage(payload))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Finding shared") {
		t.Errorf("unexpected confirmation: %q", out)
	}
	mems, err := memory.List(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected exactly 1 generated memory, got %d", len(mems))
	}
	m := mems[0]
	if !strings.HasPrefix(m.Name, "finding-") {
		t.Errorf("finding name should be distinguishable from episodic-*, got %q", m.Name)
	}
	if m.Confidence != memory.ConfidenceGenerated {
		t.Errorf("finding must be provenance-stamped confidence=generated, got %q", m.Confidence)
	}
	if m.UserAuthored() {
		t.Error("a shared finding must never count as user-authored")
	}
	// The description carries the summary + description; the secret must be gone.
	if !m.MatchesPath("internal/cli/pool_detect.go") {
		t.Errorf("paths frontmatter should route the finding to its files; paths=%v", m.Paths)
	}
	content, err := memory.Read(ws, m.Name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "SUPERSECRETVALUE") {
		t.Errorf("finding body was persisted UNREDACTED:\n%s", content)
	}
	if !strings.Contains(content, "the pool is keyed by") || !strings.Contains(content, "secondaries live to shutdown") {
		t.Errorf("finding body should carry summary and description:\n%s", content)
	}
}

// TestShareFindings_RoutedByRelevant proves a shared finding is discoverable by a
// concurrent peer through relevant_memories' underlying Relevant lookup.
func TestShareFindings_RoutedByRelevant(t *testing.T) {
	deps, ws := shareFindingsTestDeps(t, CollabPolicy{KnowledgeHandoff: true}, 50)
	tool := NewShareFindings(deps)
	payload := `{"summary":"cache invalidation is LSP-driven","paths":["internal/cache/**"]}`
	if _, err := tool.Execute(context.Background(), json.RawMessage(payload)); err != nil {
		t.Fatal(err)
	}
	hits, err := memory.Relevant(ws, "internal/cache/invalidator.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("peer should find the finding via relevant_memories; got %d hits", len(hits))
	}
}

func TestShareFindings_MissingSummaryRejected(t *testing.T) {
	deps, _ := shareFindingsTestDeps(t, CollabPolicy{KnowledgeHandoff: true}, 50)
	tool := NewShareFindings(deps)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"description":"detail only"}`)); err == nil {
		t.Fatal("expected an error for a missing summary")
	}
}

// TestShareFindings_RapidCallsDoNotCollide: two findings shared by the same
// session in immediate succession (the same wall-clock second) must produce two
// distinct memories — a second-resolution name alone would silently overwrite
// the first.
func TestShareFindings_RapidCallsDoNotCollide(t *testing.T) {
	deps, ws := shareFindingsTestDeps(t, CollabPolicy{KnowledgeHandoff: true}, 50)
	tool := NewShareFindings(deps)
	for _, s := range []string{"first finding", "second finding"} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{"summary":`+jsonStr(s)+`}`)); err != nil {
			t.Fatalf("Execute(%q): %v", s, err)
		}
	}
	mems, err := memory.List(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 {
		t.Fatalf("two rapid share_findings calls must yield two memories, got %d", len(mems))
	}
}

func TestShareFindings_NoWorkspace(t *testing.T) {
	tool := NewShareFindings(ShareFindingsDeps{
		Workspace:           func() string { return "" },
		SessionName:         func() string { return "s" },
		Policy:              func() CollabPolicy { return CollabPolicy{KnowledgeHandoff: true} },
		Index:               func() *memory.Index { return nil },
		GeneratedMemoryKeep: func() int { return 50 },
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"summary":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session_start") {
		t.Errorf("expected an attach hint; got %q", out)
	}
}
