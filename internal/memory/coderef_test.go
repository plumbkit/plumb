package memory

import "testing"

func TestMemoriesForRefs(t *testing.T) {
	mems := []Memory{
		{Name: "sym-note", SourceSymbols: []string{"AcquireLock"}, Confidence: ConfidenceGenerated},
		{Name: "glob-note", Paths: []string{"internal/auth/**"}},
		{Name: "prov-note", SourcePaths: []string{"cmd/main.go"}, Confidence: ConfidenceGenerated},
		{Name: "unrelated", Paths: []string{"docs/**"}},
	}
	refs := []CodeRef{
		{SymbolName: "AcquireLock", File: "internal/cli/lock.go"},
		{File: "internal/auth/login.go"},
		{File: "cmd/main.go"},
	}

	hits := MemoriesForRefs(mems, refs, 10)
	if len(hits) != 3 {
		t.Fatalf("want 3 hits, got %d: %+v", len(hits), hits)
	}
	// User-authored memories claim slots before generated ones.
	if hits[0].Name != "glob-note" {
		t.Errorf("user-authored memory must lead, got %q", hits[0].Name)
	}
	whys := map[string]string{}
	for _, h := range hits {
		whys[h.Name] = h.Why
	}
	if whys["sym-note"] != "references symbol AcquireLock" {
		t.Errorf("sym-note why = %q", whys["sym-note"])
	}
	if whys["glob-note"] != "paths glob matches internal/auth/login.go" {
		t.Errorf("glob-note why = %q", whys["glob-note"])
	}
	if whys["prov-note"] != "session provenance touched cmd/main.go" {
		t.Errorf("prov-note why = %q", whys["prov-note"])
	}
}

func TestMemoriesForRefs_CapAndEmpty(t *testing.T) {
	mems := []Memory{
		{Name: "a", Paths: []string{"**"}},
		{Name: "b", Paths: []string{"**"}},
	}
	refs := []CodeRef{{File: "x.go"}}

	if hits := MemoriesForRefs(mems, refs, 1); len(hits) != 1 {
		t.Errorf("max=1 must cap, got %d", len(hits))
	}
	if hits := MemoriesForRefs(mems, nil, 5); hits != nil {
		t.Errorf("no refs must yield nil, got %+v", hits)
	}
	if hits := MemoriesForRefs(nil, refs, 5); hits != nil {
		t.Errorf("no memories must yield nil, got %+v", hits)
	}
	if hits := MemoriesForRefs(mems, refs, 0); hits != nil {
		t.Errorf("max=0 must yield nil, got %+v", hits)
	}
}

// TestList_PopulatesSourceRefs: List surfaces provenance source_paths and
// source_symbols from frontmatter, so the hint path and the CodeRef join can
// match on them without reading bodies or the index.
func TestList_PopulatesSourceRefs(t *testing.T) {
	ws := t.TempDir()
	prov := Provenance{
		SourcePaths:   []string{"internal/cli/lock.go"},
		SourceSymbols: []string{"AcquireLock", "ReleaseLock"},
	}
	if err := WriteGenerated(nil, ws, "lock-episode", "d", "body", prov); err != nil {
		t.Fatal(err)
	}
	mems, err := List(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("want 1 memory, got %d", len(mems))
	}
	m := mems[0]
	if len(m.SourcePaths) != 1 || m.SourcePaths[0] != "internal/cli/lock.go" {
		t.Errorf("SourcePaths = %v", m.SourcePaths)
	}
	if len(m.SourceSymbols) != 2 || m.SourceSymbols[0] != "AcquireLock" {
		t.Errorf("SourceSymbols = %v", m.SourceSymbols)
	}
}
