package cli

// conn_semantics.go — resolves the session's [semantics] config into the
// per-call SemanticRerankConfig that topology_search consumes.

import (
	"github.com/plumbkit/plumb/internal/semantics"
	"github.com/plumbkit/plumb/internal/tools"
)

// semanticRerank builds the live semantic re-rank config for topology_search.
// Read off the lock-free snapshot, so it honours hot-reloaded [semantics] config.
// Returns a disabled config (Embedder nil) when semantics is off, no API key
// resolves for a hosted provider, or the embedder cannot be built — in which
// case topology_search falls back to the FTS5 baseline.
func (s *connSession) semanticRerank() tools.SemanticRerankConfig {
	cfg := s.view().semantics
	if !cfg.Enabled {
		return tools.SemanticRerankConfig{}
	}
	r := cfg.Resolve()
	// A hosted provider needs a key; a custom (self-run) endpoint usually doesn't.
	// Skip building the embedder when a hosted provider has no key, so we don't
	// fire a doomed request on every search.
	if r.APIKey == "" && r.Provider != "custom" {
		return tools.SemanticRerankConfig{Enabled: true, Candidates: r.RerankCandidates}
	}
	emb, err := semantics.NewEmbedder(r.Provider, r.BaseURL, r.Model, r.APIKey, r.Timeout)
	if err != nil {
		s.log().Warn("semantics: embedder unavailable; topology_search using FTS5 only", "err", err)
		return tools.SemanticRerankConfig{Enabled: true, Candidates: r.RerankCandidates}
	}
	return tools.SemanticRerankConfig{Enabled: true, Candidates: r.RerankCandidates, Embedder: emb}
}
