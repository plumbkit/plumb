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

// TestWriteGenerated_PathsNewlineCannotInjectFrontmatter: a provenance value
// carrying a newline must not terminate its frontmatter line and smuggle in a
// key of its own — the list writer scrubs newlines to spaces.
func TestWriteGenerated_PathsNewlineCannotInjectFrontmatter(t *testing.T) {
	ws := t.TempDir()
	prov := Provenance{SourcePaths: []string{"a.go\nstale_after: 2020-01-01T00:00:00Z"}}
	if err := WriteGenerated(nil, ws, "inject", "d", "body", prov); err != nil {
		t.Fatalf("WriteGenerated: %v", err)
	}
	rec, err := ReadMeta(ws, "inject")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if !rec.StaleAfter.IsZero() {
		t.Errorf("newline in a list value injected stale_after = %v", rec.StaleAfter)
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

// TestPruneGeneratedEpisodic_FindingsShareRetentionPool: an on-demand
// share_findings memory ("finding-") competes for the same generated_memory_keep
// cap as idle episodic summaries — one shared pool, ordered purely by age.
func TestPruneGeneratedEpisodic_FindingsShareRetentionPool(t *testing.T) {
	ws := t.TempDir()
	names := []string{"episodic-old", "finding-mid", "episodic-new"}
	for i, name := range names {
		created := time.Date(2026, 7, 1, 12, i, 0, 0, time.UTC)
		if err := WriteGenerated(nil, ws, name, "d", "body", Provenance{Confidence: ConfidenceGenerated, CreatedAt: created}); err != nil {
			t.Fatalf("WriteGenerated(%s): %v", name, err)
		}
	}

	// keep=2: the finding outranks the older episodic summary and is retained.
	deleted, err := PruneGeneratedEpisodic(nil, ws, 2)
	if err != nil {
		t.Fatalf("PruneGeneratedEpisodic(keep=2): %v", err)
	}
	if deleted != 1 {
		t.Fatalf("keep=2 deleted = %d, want 1", deleted)
	}
	if _, err := Read(ws, "episodic-old"); err == nil {
		t.Error("episodic-old (oldest in the shared pool) should have been pruned")
	}
	if _, err := Read(ws, "finding-mid"); err != nil {
		t.Errorf("finding-mid should remain under keep=2: %v", err)
	}

	// keep=1: the finding is itself eligible and pruned like any generated memory.
	deleted, err = PruneGeneratedEpisodic(nil, ws, 1)
	if err != nil {
		t.Fatalf("PruneGeneratedEpisodic(keep=1): %v", err)
	}
	if deleted != 1 {
		t.Fatalf("keep=1 deleted = %d, want 1", deleted)
	}
	if _, err := Read(ws, "finding-mid"); err == nil {
		t.Error("finding-mid should have been pruned under keep=1")
	}
	if _, err := Read(ws, "episodic-new"); err != nil {
		t.Errorf("episodic-new (newest) should always remain: %v", err)
	}
}

// TestPruneGeneratedEpisodic_ZeroKeepDisables: keep <= 0 must delete nothing.
func TestPruneGeneratedEpisodic_ZeroKeepDisables(t *testing.T) {
	ws := t.TempDir()
	for _, name := range []string{"episodic-a", "episodic-b"} {
		if err := WriteGenerated(nil, ws, name, "d", "body", Provenance{}); err != nil {
			t.Fatalf("WriteGenerated(%s): %v", name, err)
		}
	}
	deleted, err := PruneGeneratedEpisodic(nil, ws, 0)
	if err != nil {
		t.Fatalf("PruneGeneratedEpisodic: %v", err)
	}
	if deleted != 0 {
		t.Errorf("keep=0 must disable pruning, deleted %d", deleted)
	}
	if mems, _ := List(ws); len(mems) != 2 {
		t.Errorf("expected both memories to remain, got %d", len(mems))
	}
}

// TestPruneGeneratedEpisodic_MissingCreatedAtFallsBackToMtime: a generated
// episodic memory whose frontmatter lacks created_at must be aged by file mtime,
// not treated as infinitely old (the zero time's UnixNano is hugely negative).
func TestPruneGeneratedEpisodic_MissingCreatedAtFallsBackToMtime(t *testing.T) {
	ws := t.TempDir()
	// A legacy generated memory with no created_at line; its file mtime is "now".
	legacy := "---\nname: episodic-legacy\nconfidence: generated\n---\n\nbody"
	if err := Write(ws, "episodic-legacy", legacy, ""); err != nil {
		t.Fatalf("Write legacy: %v", err)
	}
	// A properly-stamped memory created yesterday — genuinely older than legacy.
	prov := Provenance{CreatedAt: time.Now().Add(-24 * time.Hour)}
	if err := WriteGenerated(nil, ws, "episodic-dated", "d", "body", prov); err != nil {
		t.Fatalf("WriteGenerated: %v", err)
	}
	deleted, err := PruneGeneratedEpisodic(nil, ws, 1)
	if err != nil {
		t.Fatalf("PruneGeneratedEpisodic: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if _, err := Read(ws, "episodic-legacy"); err != nil {
		t.Errorf("legacy (newest by mtime) should remain: %v", err)
	}
	if _, err := Read(ws, "episodic-dated"); err == nil {
		t.Error("episodic-dated (older by created_at) should have been pruned")
	}
}
