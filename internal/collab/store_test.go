package collab

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	ws := t.TempDir()
	s, err := Open(ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, ws
}

func TestExists_LazyCreation(t *testing.T) {
	ws := t.TempDir()
	if Exists(ws) {
		t.Fatal("Exists should be false before any collab feature is used")
	}
	s, err := Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !Exists(ws) {
		t.Fatal("Exists should be true after Open")
	}
}

func TestOpen_WritesGitignore(t *testing.T) {
	s, ws := openTestStore(t)
	_ = s
	data, err := os.ReadFile(filepath.Join(ws, ".plumb", ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	for _, want := range []string{"collab.db", "collab.db-wal", "collab.db-shm"} {
		if !strings.Contains(string(data), want) {
			t.Errorf(".gitignore missing %q; got:\n%s", want, data)
		}
	}
}

func TestPutIntent_ReplacesPriorPerSession(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	put := func(id, body string) {
		if err := s.PutIntent(ctx, IntentInput{AuthorSession: "sess-" + id, AuthorID: id, Body: body, TTL: time.Hour}, now); err != nil {
			t.Fatalf("PutIntent: %v", err)
		}
	}
	put("A", "first")
	put("A", "second") // replaces A's intent
	put("B", "other")

	intents, err := s.LiveIntents(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 2 {
		t.Fatalf("live intents = %d, want 2 (one per session)", len(intents))
	}
	bodies := map[string]string{}
	for _, r := range intents {
		bodies[r.AuthorID] = r.Body
	}
	if bodies["A"] != "second" {
		t.Errorf("A's live intent = %q, want the replacement %q", bodies["A"], "second")
	}
	if bodies["B"] != "other" {
		t.Errorf("B's live intent = %q, want %q", bodies["B"], "other")
	}
}

func TestLiveIntents_FiltersExpired(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.PutIntent(ctx, IntentInput{AuthorID: "A", Body: "x", TTL: 5 * time.Minute}, now); err != nil {
		t.Fatal(err)
	}
	// Query as if 10 minutes have passed — the row is expired and filtered.
	future := now.Add(10 * time.Minute)
	intents, err := s.LiveIntents(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Fatalf("expired intent should be filtered on read; got %d", len(intents))
	}
}

func TestDeliverNotes_ConsumesNextButKeepsAddressed(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	mustNote := func(to, body string) {
		if err := s.PutNote(ctx, NoteInput{AuthorSession: "author", AuthorID: "au", Body: body, Addressee: to, TTL: time.Hour}, now); err != nil {
			t.Fatalf("PutNote: %v", err)
		}
	}
	mustNote(AddresseeNext, "for whoever attaches next")
	mustNote("alice", "hello alice")

	// alice attaches first: she gets her note + the next note; the next note is consumed.
	got, err := s.DeliverNotes(ctx, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("alice delivery = %d notes, want 2 (addressed + next)", len(got))
	}

	// bob attaches later: the next note is gone; alice's addressed note is not his.
	got2, err := s.DeliverNotes(ctx, "bob", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 0 {
		t.Fatalf("bob delivery = %d notes, want 0 (next consumed, addressed note not his)", len(got2))
	}

	// alice's addressed note persists (delivered again until TTL), non-consumed.
	pending, err := s.PendingNotes(ctx, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != "hello alice" {
		t.Fatalf("alice pending = %v, want her addressed note still present", pending)
	}
}

func TestPendingNotes_AddresseeMatchOnly(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_ = s.PutNote(ctx, NoteInput{AuthorID: "au", Body: "n1", Addressee: "alice", TTL: time.Hour}, now)
	_ = s.PutNote(ctx, NoteInput{AuthorID: "au", Body: "n2", Addressee: AddresseeNext, TTL: time.Hour}, now)

	pending, err := s.PendingNotes(ctx, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending for alice = %d, want 1 (a next note is not listed here)", len(pending))
	}
	if pending[0].Addressee != "alice" {
		t.Errorf("pending addressee = %q, want alice", pending[0].Addressee)
	}
}

func TestPutNote_DefaultsToNext(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.PutNote(ctx, NoteInput{AuthorID: "au", Body: "hi", Addressee: "", TTL: time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.DeliverNotes(ctx, "whoever", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Addressee != AddresseeNext {
		t.Fatalf("empty addressee should default to %q; got %v", AddresseeNext, got)
	}
}

func TestPrune_RemovesExpired(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_ = s.PutIntent(ctx, IntentInput{AuthorID: "A", Body: "x", TTL: 5 * time.Minute}, now)
	_ = s.PutNote(ctx, NoteInput{AuthorID: "A", Body: "y", Addressee: "bob", TTL: 5 * time.Minute}, now)

	n, err := s.Prune(ctx, now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d rows, want 2", n)
	}
	// A fresh row survives a prune at present time.
	_ = s.PutIntent(ctx, IntentInput{AuthorID: "B", Body: "z", TTL: time.Hour}, now)
	n2, err := s.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("pruned %d unexpired rows, want 0", n2)
	}
}

func TestClearSessionIntents_LeavesNotes(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_ = s.PutIntent(ctx, IntentInput{AuthorID: "sess1", Body: "refactoring", TTL: time.Hour}, now)
	_ = s.PutNote(ctx, NoteInput{AuthorID: "sess1", Body: "note survives", Addressee: "peer", TTL: time.Hour}, now)

	if err := s.ClearSessionIntents(ctx, "sess1"); err != nil {
		t.Fatal(err)
	}
	intents, _ := s.LiveIntents(ctx, now)
	if len(intents) != 0 {
		t.Fatalf("session's intent should be cleared on close; got %d", len(intents))
	}
	notes, _ := s.PendingNotes(ctx, "peer", now)
	if len(notes) != 1 {
		t.Fatalf("notes must survive their author; got %d", len(notes))
	}
}

func TestClampTTL_FloorsShortTTL(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	// A zero TTL would store an already-expired row; clampTTL floors it to minTTL.
	if err := s.PutIntent(ctx, IntentInput{AuthorID: "A", Body: "x", TTL: 0}, now); err != nil {
		t.Fatal(err)
	}
	intents, err := s.LiveIntents(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 {
		t.Fatalf("a zero-TTL intent should still live at least minTTL; got %d", len(intents))
	}
}
