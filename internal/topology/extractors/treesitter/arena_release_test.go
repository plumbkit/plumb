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
//
// It sweeps EVERY extractor in the package (allExtractorCases) rather than
// Python alone, because a single-extractor guard cannot see a missing release
// elsewhere: the pure-Go TS/TSX extractor landed without its
// `defer tree.Release()` and burned a fresh arena on every parse while this test
// stayed green.
func TestExtractReleasesArenaForReuse(t *testing.T) {
	// The signal is binary — a released tree recycles a single arena for the whole
	// run, an unreleased one allocates a fresh arena per parse — so a small n
	// separates them as sharply as a large one and keeps this sweep cheap.
	const n = 10
	for _, tc := range allExtractorCases() {
		t.Run(tc.name, func(t *testing.T) {
			ext := tc.ctor()
			tsg.DrainArenaPools()
			tsg.EnableArenaProfile(true)
			defer tsg.EnableArenaProfile(false)
			tsg.ResetArenaProfile()

			src := []byte(tc.src)
			for i := range n {
				nodes, _, err := ext.Extract(context.Background(), tc.path, src)
				if err != nil {
					t.Fatalf("extract %d: %v", i, err)
				}
				// A sample that extracts nothing would make the counters below
				// meaningless and silently skip on the acquires check.
				if i == 0 && len(nodes) == 0 {
					t.Fatalf("the %s sample extracted no symbols — fix it in allExtractorCases", tc.name)
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
		})
	}
}
