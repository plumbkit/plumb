package memory

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// TestIndex_ReindexAsyncSelfHeals: a memory written to disk behind the index
// makes it stale; ReindexAsync brings it fresh in the background.
func TestIndex_ReindexAsyncSelfHeals(t *testing.T) {
	ws := t.TempDir()
	ix, err := OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	if err := Write(ws, "fresh", "alpha-zebra-token", "d"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fresh, _ := ix.Fresh(ws); fresh {
		t.Fatal("expected the index to be stale before reindex")
	}
	ix.ReindexAsync(ws)
	deadline := time.After(3 * time.Second)
	for {
		if fresh, _ := ix.Fresh(ws); fresh {
			return
		}
		select {
		case <-deadline:
			t.Fatal("ReindexAsync did not make the index fresh")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestIndex_ReindexAsyncConcurrent: the CAS guard admits one runner; many
// concurrent callers are safe (run under -race).
func TestIndex_ReindexAsyncConcurrent(t *testing.T) {
	ws := t.TempDir()
	ix, err := OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	if err := Write(ws, "m", "body", "d"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); ix.ReindexAsync(ws) }()
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond) // let any in-flight reindex finish before Close
}

// TestIndex_CloseRaceWithReindex: Close must not race or panic against a
// concurrent Reindex on the same handle (run under -race). A Close that wins
// leaves a closed handle; the Reindex then errors cleanly (never nil-derefs).
func TestIndex_CloseRaceWithReindex(t *testing.T) {
	for range 20 {
		ws := t.TempDir()
		ix, err := OpenIndex(ws)
		if err != nil {
			t.Fatalf("OpenIndex: %v", err)
		}
		if err := Write(ws, "m", "some body content", "d"); err != nil {
			t.Fatalf("Write: %v", err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = ix.Reindex(ws) }()
		go func() { defer wg.Done(); _ = ix.Close() }()
		wg.Wait()
	}
}

// The code-aware tokenisation that backs memory FTS now lives in
// internal/tokenise (shared with topology); its canonical table test moved
// there. TestIndex_UpsertAndSearch below proves the integration end-to-end.

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

// recordTimes reads the stored created_at / last_used_at for a memory directly,
// for asserting timestamp preservation across a re-upsert.
func recordTimes(t *testing.T, ix *Index, name string) (createdNS, lastUsedNS int64) {
	t.Helper()
	err := ix.db.QueryRow(`SELECT created_at, last_used_at FROM memory_records WHERE name = ?`, name).
		Scan(&createdNS, &lastUsedNS)
	if err != nil {
		t.Fatalf("recordTimes %q: %v", name, err)
	}
	return createdNS, lastUsedNS
}

// TestIndex_PriorTimesPreservedOnReupsert: re-indexing a memory (no explicit
// CreatedAt) must keep its original created_at and last_used_at — a re-upsert is
// not a "new" memory, so recency/age ranking signals survive a content edit.
func TestIndex_PriorTimesPreservedOnReupsert(t *testing.T) {
	ix, _ := openTestIndex(t)
	if err := ix.Upsert(Record{Name: "note", Description: "v1", Body: "first body"}); err != nil {
		t.Fatalf("Upsert v1: %v", err)
	}
	created1, _ := recordTimes(t, ix, "note")
	// Bump last_used_at so we can prove it is preserved (not reset) by re-upsert.
	if err := ix.TouchUsed("note"); err != nil {
		t.Fatalf("TouchUsed: %v", err)
	}
	_, touched := recordTimes(t, ix, "note")
	if touched == 0 {
		t.Fatal("TouchUsed did not set last_used_at")
	}

	if err := ix.Upsert(Record{Name: "note", Description: "v2", Body: "second body"}); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}
	created2, lastUsed2 := recordTimes(t, ix, "note")
	if created2 != created1 {
		t.Errorf("created_at not preserved across re-upsert: %d -> %d", created1, created2)
	}
	if lastUsed2 != touched {
		t.Errorf("last_used_at not preserved across re-upsert: %d -> %d", touched, lastUsed2)
	}
}

// TestIndex_TouchUsedBumpsLastUsed: TouchUsed strictly advances last_used_at.
func TestIndex_TouchUsedBumpsLastUsed(t *testing.T) {
	ix, _ := openTestIndex(t)
	if err := ix.Upsert(Record{Name: "note", Body: "body"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_, before := recordTimes(t, ix, "note")
	time.Sleep(2 * time.Millisecond) // ensure a distinct nanosecond clock reading
	if err := ix.TouchUsed("note"); err != nil {
		t.Fatalf("TouchUsed: %v", err)
	}
	_, after := recordTimes(t, ix, "note")
	if after <= before {
		t.Errorf("TouchUsed did not advance last_used_at: %d -> %d", before, after)
	}
	// A missing memory is a no-op, not an error.
	if err := ix.TouchUsed("does-not-exist"); err != nil {
		t.Errorf("TouchUsed on missing memory should be a no-op, got %v", err)
	}
}

// TestIndex_RecencyTiebreak: two memories of equal rank and confidence are
// ordered by last_used_at DESC — touching the lower one floats it to the top.
func TestIndex_RecencyTiebreak(t *testing.T) {
	ix, _ := openTestIndex(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Identical body/description so FTS rank ties; same (default) confidence.
	must(ix.Upsert(Record{Name: "alpha", Description: "auth notes", Body: "auth auth auth"}))
	must(ix.Upsert(Record{Name: "bravo", Description: "auth notes", Body: "auth auth auth"}))

	time.Sleep(2 * time.Millisecond)
	must(ix.TouchUsed("bravo")) // make bravo strictly more recently used

	hits, err := ix.Search(context.Background(), "auth", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].Name != "bravo" {
		t.Errorf("recency tiebreak: more-recently-used 'bravo' should sort first, got %q then %q",
			hits[0].Name, hits[1].Name)
	}
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
