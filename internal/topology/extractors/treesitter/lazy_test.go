package treesitter

import (
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// TestLazyGrammarMemoisesAndDefers proves lazyGrammar defers its load until the
// first get and runs it exactly once thereafter.
func TestLazyGrammarMemoisesAndDefers(t *testing.T) {
	calls := 0
	g := lazyGrammar{load: func() *tsg.Language { calls++; return nil }}
	if calls != 0 {
		t.Fatalf("load ran before get(): %d calls", calls)
	}
	g.get()
	g.get()
	g.get()
	if calls != 1 {
		t.Fatalf("load ran %d times across 3 get() calls; want 1 (memoised)", calls)
	}
}

// TestExtractorConstructionDecodesNoGrammar is the regression guard for the
// memory win: buildExtractors constructs an extractor for every tree-sitter
// language on every workspace attach, but constructing them must decode NO
// grammar — each grammar (tens of MB) loads only when a file of that language
// is first extracted. The package has no parallel tests, so the process-global
// grammar cache is stable across this test.
func TestExtractorConstructionDecodesNoGrammar(t *testing.T) {
	grammars.PurgeEmbeddedLanguageCache()
	if loaded, _ := grammars.EmbeddedLanguageCacheStats(); loaded != 0 {
		t.Fatalf("grammar cache not empty after purge: %d", loaded)
	}

	// Every extractor in the package, from the shared enumeration — the list that
	// used to live here went stale and omitted TS/TSX, which then shipped decoding
	// both grammars eagerly with this guard still green.
	for _, c := range allExtractorCases() {
		_ = c.ctor()
	}

	if loaded, _ := grammars.EmbeddedLanguageCacheStats(); loaded != 0 {
		t.Fatalf("constructing the tree-sitter extractors eagerly decoded %d grammars; want 0 — grammars must load lazily on first Extract", loaded)
	}
}
