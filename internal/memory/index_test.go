package memory

import (
	"context"
	"os"
	"testing"
)

// TestSplitIdentifier locks the code-aware tokenisation. The expected outputs
// mirror internal/topology's splitIdentifier (the copy's reference behaviour).
func TestSplitIdentifier(t *testing.T) {
	cases := map[string]string{
		"UserSession":   "user session",
		"workspacePool": "workspace pool",
		"HTTPServer":    "http server",
		"user_session":  "user session",
		"auth-gotchas":  "auth gotchas",
		"internal/auth": "internal auth",
		"":              "",
	}
	for in, want := range cases {
		if got := splitIdentifier(in); got != want {
			t.Errorf("splitIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func openTestIndex(t *testing.T) (*Index, string) {
	t.Helper()
	ws := t.TempDir()
	ix, err := OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix, ws
}

func TestIndex_UpsertAndSearch(t *testing.T) {
	ix, _ := openTestIndex(t)
	if err := ix.Upsert(Record{
		Name: "UserSession", Description: "session handling",
		Body: "manages the user session lifecycle", Confidence: ConfidenceUser,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// CamelCase query matches via name_tokens.
	hits, err := ix.Search(context.Background(), "user session", SearchOpts{Limit: 10, Snippets: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Name != "UserSession" {
		t.Fatalf("expected UserSession hit, got %+v", hits)
	}
	if hits[0].Snippet == "" {
		t.Error("expected a snippet")
	}
}

func TestIndex_UserRanksAboveGenerated(t *testing.T) {
	ix, _ := openTestIndex(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ix.Upsert(Record{Name: "auth-generated", Description: "auth notes", Body: "auth auth auth", Confidence: ConfidenceGenerated}))
	must(ix.Upsert(Record{Name: "auth-user", Description: "auth notes", Body: "auth auth auth", Confidence: ConfidenceUser}))

	hits, err := ix.Search(context.Background(), "auth", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].Name != "auth-user" {
		t.Errorf("user-authored memory should rank first, got %q (%s) then %q",
			hits[0].Name, hits[0].Confidence, hits[1].Name)
	}
}

func TestIndex_FreshReindexAndRemove(t *testing.T) {
	ix, ws := openTestIndex(t)
	ctx := context.Background()

	if err := Write(ws, "notes", "# Notes\n\nthe UserSession lifecycle", "session notes"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := ix.Reindex(ws); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	fresh, err := ix.Fresh(ws)
	if err != nil || !fresh {
		t.Fatalf("expected fresh after reindex, got fresh=%v err=%v", fresh, err)
	}
	hits, _ := ix.Search(ctx, "session", SearchOpts{Limit: 10})
	if len(hits) == 0 {
		t.Fatal("expected the reindexed memory to be searchable")
	}

	// Modify on disk → stale → reindex repairs.
	if err := Write(ws, "notes", "# Notes\n\ncompletely different content about caching layers", "caching"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if fresh, _ := ix.Fresh(ws); fresh {
		t.Error("expected stale after out-of-band modification")
	}
	if _, err := ix.Reindex(ws); err != nil {
		t.Fatalf("Reindex 2: %v", err)
	}
	if fresh, _ := ix.Fresh(ws); !fresh {
		t.Error("expected fresh after repair reindex")
	}

	// Delete on disk → reindex removes it from search.
	p, _ := Path(ws, "notes")
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := ix.Reindex(ws); err != nil {
		t.Fatalf("Reindex 3: %v", err)
	}
	if hits, _ := ix.Search(ctx, "caching", SearchOpts{Limit: 10}); len(hits) != 0 {
		t.Errorf("expected no hits after the memory was deleted, got %d", len(hits))
	}
}

func TestIndex_RemoveDropsFromSearch(t *testing.T) {
	ix, _ := openTestIndex(t)
	ctx := context.Background()
	if err := ix.Upsert(Record{Name: "doomed", Body: "unique-token-zebra", Confidence: ConfidenceUser}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if hits, _ := ix.Search(ctx, "zebra", SearchOpts{Limit: 5}); len(hits) != 1 {
		t.Fatalf("expected 1 hit before remove, got %d", len(hits))
	}
	if err := ix.Remove("doomed"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if hits, _ := ix.Search(ctx, "zebra", SearchOpts{Limit: 5}); len(hits) != 0 {
		t.Errorf("expected 0 hits after remove, got %d", len(hits))
	}
}
