package treesitter

import (
	"sync"

	tsg "github.com/odvcencio/gotreesitter"
)

// lazyGrammar defers a gotreesitter grammar decode until first use. Constructing
// an extractor no longer forces its grammar (tens of MB of transition tables) to
// decode up front; the decode happens only when a file of that language is first
// indexed. A workspace that never contains, say, Swift therefore never pays
// Swift's grammar cost — important because buildExtractors instantiates an
// extractor for every supported language on every workspace attach, regardless
// of which languages the workspace actually holds.
//
// The underlying grammars.*Language is itself process-cached (decode-once,
// shared across extractors and workspaces), so this changes only *when* the one
// decode happens, never how many times. get is safe for concurrent use; the
// owning extractor must be used by pointer so the embedded sync.Once is not
// copied after first use.
type lazyGrammar struct {
	load func() *tsg.Language
	once sync.Once
	lang *tsg.Language
}

func (g *lazyGrammar) get() *tsg.Language {
	g.once.Do(func() { g.lang = g.load() })
	return g.lang
}
