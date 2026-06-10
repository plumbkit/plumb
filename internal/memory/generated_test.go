package memory

import (
	"context"
	"testing"
	"time"
)

// TestWriteGenerated_EmitsPathsGlob proves a generated memory carries a `paths:`
// frontmatter line derived (deduped) from its provenance SourcePaths, so it
// hint-matches the files it was distilled from — round-tripping through List and
// MatchesPath exactly like a hand-written memory.
func TestWriteGenerated_EmitsPathsGlob(t *testing.T) {
	ws := t.TempDir()
	prov := Provenance{
		SourcePaths: []string{"internal/auth/login.go", "internal/auth/login.go", "cmd/server/main.go"},
	}
	if err := WriteGenerated(nil, ws, "auth-insight", "what we learned", "body text", prov); err != nil {
		t.Fatalf("WriteGenerated: %v", err)
	}
	mems, err := List(ws)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}
	m := mems[0]
	if len(m.Paths) != 2 {
		t.Fatalf("expected 2 deduped paths, got %v", m.Paths)
	}
	if !m.MatchesPath("internal/auth/login.go") {
		t.Errorf("generated memory should hint-match its source path; paths=%v", m.Paths)
	}
	if !m.MatchesPath("cmd/server/main.go") {
		t.Errorf("generated memory should match the second source path; paths=%v", m.Paths)
	}
	if m.MatchesPath("internal/db/store.go") {
		t.Errorf("unrelated path must not match; paths=%v", m.Paths)
	}
}

// TestWriteGenerated_NoPathsWhenNoSources: with no SourcePaths there is no
// `paths:` line, so the memory matches nothing by path.
func TestWriteGenerated_NoPathsWhenNoSources(t *testing.T) {
	ws := t.TempDir()
	if err := WriteGenerated(nil, ws, "general", "d", "body", Provenance{}); err != nil {
		t.Fatalf("WriteGenerated: %v", err)
	}
	mems, _ := List(ws)
	if len(mems) != 1 || len(mems[0].Paths) != 0 {
		t.Fatalf("expected a memory with no paths, got %+v", mems)
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"a", "", "a", " b ", "b", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupeStrings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeStrings[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

// TestMatchMemoryField_SourcePathsLabel: a query term present only in the
// source_paths column is labelled source_paths, not the "name" default.
func TestMatchMemoryField_SourcePathsLabel(t *testing.T) {
	// query, name, tokens, desc, body, globs, spaths, ssyms
	if f := matchMemoryField("payments", "auth-notes", "auth notes", "login flow", "some body",
		"internal/auth/**", "internal/payments/charge.go", ""); f != "source_paths" {
		t.Errorf("expected source_paths label, got %q", f)
	}
	if f := matchMemoryField("Charger", "auth-notes", "auth notes", "login flow", "some body",
		"internal/auth/**", "", "Charger"); f != "source_symbols" {
		t.Errorf("expected source_symbols label, got %q", f)
	}
	// Body still wins only when nothing earlier matches.
	if f := matchMemoryField("zebra", "auth-notes", "auth notes", "login flow", "a zebra in body",
		"internal/auth/**", "", ""); f != "body" {
		t.Errorf("expected body label, got %q", f)
	}
}

// TestWriteGenerated_IndexedSourcePathsSearchable: a generated memory's source
// path is reachable through the FTS index and labelled source_paths.
func TestWriteGenerated_IndexedSourcePathsSearchable(t *testing.T) {
	ws := t.TempDir()
	ix, err := OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	prov := Provenance{SourcePaths: []string{"internal/payments/charge.go"}}
	if err := WriteGenerated(ix, ws, "pay-insight", "payments learning", "body", prov); err != nil {
		t.Fatalf("WriteGenerated: %v", err)
	}
	hits, err := ix.Search(context.Background(), "charge.go", SearchOpts{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected the generated memory to be searchable by source path")
	}
	if hits[0].Field != "source_paths" && hits[0].Field != "path" {
		t.Errorf("expected source_paths/path field label, got %q", hits[0].Field)
	}
}

func TestPruneGeneratedEpisodic_OnlyDeletesOldGeneratedEpisodic(t *testing.T) {
	ws := t.TempDir()
	ix, err := OpenIndex(ws)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer ix.Close()
	for i, name := range []string{"episodic-old", "episodic-new"} {
		created := time.Date(2026, 6, 9, 12, i, 0, 0, time.UTC)
		if err := WriteGenerated(ix, ws, name, "session", "body", Provenance{CreatedAt: created}); err != nil {
			t.Fatalf("WriteGenerated(%s): %v", name, err)
		}
	}
	if err := Write(ws, "episodic-user", "user body", "user desc"); err != nil {
		t.Fatalf("Write user memory: %v", err)
	}
	if err := WriteGenerated(ix, ws, "general-generated", "general", "body", Provenance{}); err != nil {
		t.Fatalf("WriteGenerated general: %v", err)
	}
	deleted, err := PruneGeneratedEpisodic(ix, ws, 1)
	if err != nil {
		t.Fatalf("PruneGeneratedEpisodic: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if _, err := Read(ws, "episodic-old"); err == nil {
		t.Fatal("episodic-old should have been pruned")
	}
	for _, name := range []string{"episodic-new", "episodic-user", "general-generated"} {
		if _, err := Read(ws, name); err != nil {
			t.Fatalf("%s should remain: %v", name, err)
		}
	}
}
