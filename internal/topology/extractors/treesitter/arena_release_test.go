package treesitter

import (
	"context"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
)

// TestExtractReleasesArenaForReuse proves Extract returns its parse-tree arena
// to gotreesitter's pool (via tree.Release), so repeated parses reuse arenas
// instead of allocating a fresh one per file. Without the release, a topology
// resync allocated a parse arena for every file — the dominant startup-transient
// allocator (~1.6 GB cumulative on this repo). The arena profile is process-
// global and not concurrency-safe; the package has no parallel tests, so this
// runs in isolation.
func TestExtractReleasesArenaForReuse(t *testing.T) {
	tsg.DrainArenaPools()
	tsg.EnableArenaProfile(true)
	defer tsg.EnableArenaProfile(false)
	tsg.ResetArenaProfile()

	ext := NewPython()
	src := []byte("def f():\n    return g()\n\n\ndef g():\n    pass\n")
	const n = 50
	for i := range n {
		if _, _, err := ext.Extract(context.Background(), "a.py", src); err != nil {
			t.Fatalf("extract %d: %v", i, err)
		}
	}

	p := tsg.ArenaProfileSnapshot()
	acquires := p.FullAcquire + p.IncrementalAcquire
	news := p.FullNew + p.IncrementalNew
	if acquires < n {
		t.Skipf("arena profile recorded only %d acquires across %d parses; the parser may not use the profiled path on this version", acquires, n)
	}
	// With recycling, newly-allocated arenas stay near the pool's working set
	// (a small constant) rather than scaling with the number of parses.
	if news >= uint64(n) {
		t.Fatalf("arena not reused: %d newly-allocated arenas across %d parses (want far below %d) — tree.Release() is not recycling arenas", news, n, n)
	}
}
