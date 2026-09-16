package cli

import "slices"

// Discovering the OTHER languages of a root that already has one of its own.
//
// resolvePrimaryLSP had two halves that behaved differently for no reason a user
// could see. A root with no language of its own got child-marker discovery and a
// markerless census, and listed everything it found. A root WITH a language
// acquired that one server and returned, so `go.mod` beside `web/tsconfig.json`
// and a `tools/` Python tree reported "Go" alone: the siblings reached no
// session_start identity line, no TUI badge, no workspace_symbols fan-out and no
// run_task reachability. Per-file routing served them lazily the whole time, so
// the defect was attach-time visibility in both halves — this is the mirror of
// the markerless census, applied to the half that still stopped early.

// siblingLanguages returns the languages of a marker-carrying root OTHER than
// its own: strong markers in subdirectories, plus a census of the remainder no
// marker claims. Nil when the feature is off, when the root is a home directory,
// or when nothing else is found.
//
// primary is the root's own language, excluded from the result and passed to the
// census as already-claimed so its files cannot nominate it a second time at a
// second root. The returned entries are never elected primary — see the call
// site — so a sibling can be surfaced and fanned out without changing which
// server answers a URI-less query.
func (s *connSession) siblingLanguages(folder, primary string) []discoveredRoot {
	cfg := s.store.Current()
	if !cfg.Workspace.DiscoverSiblings {
		return nil
	}
	// The same guard the LanguageNone path applies, for the same reason: a stray
	// ~/.plumb must never trigger a descent into the whole home directory. It is
	// repeated here rather than hoisted because the two paths reach this point by
	// different routes, and a guard that protects only one of them is the shape of
	// bug this whole change is about.
	if sameDirAs(folder, homeDirInfos()) {
		return nil
	}
	// The root's own language is claimed at the root itself. The census reads
	// claimed roots to prune subtrees and claimed LANGUAGES to skip nominations;
	// passing the primary this way does both, so the root's own sources cannot
	// nominate a duplicate entry for a language that already has a server.
	found := s.pool.discoverChildLanguages(folder, cfg.Workspace.ChildScanDepth)
	claimed := make([]discoveredRoot, 0, len(found)+1)
	claimed = append(claimed, discoveredRoot{root: folder, language: primary})
	claimed = append(claimed, found...)
	found = append(found, s.pool.censusMarkerlessLanguages(folder, claimed)...)

	out := make([]discoveredRoot, 0, len(found))
	for _, d := range found {
		// A child root whose language IS the primary is dropped rather than
		// listed: the primary's own server is already bound at the workspace root
		// and covers it by containment, so listing it again would add a duplicate
		// adapter to the session record and a redundant fan-out target for a
		// language that is already answering.
		if d.language == primary {
			continue
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// discoveredWithPrimary prepends the root's own marker-backed language to a
// sibling set, for the surfaces that describe what the WHOLE workspace holds —
// the session_start identity line, the TUI badge, the adapter list, the
// workspace_symbols fan-out. Without it those surfaces would name every language
// except the one actually bound as primary.
//
// Separate from siblingLanguages because the two answer different questions:
// "what else is here?" (which must exclude the primary, or it would be elected
// against itself) and "what does this workspace hold?" (which must include it).
func discoveredWithPrimary(root, primary string, siblings []discoveredRoot) []discoveredRoot {
	if len(siblings) == 0 {
		return nil
	}
	out := make([]discoveredRoot, 0, len(siblings)+1)
	out = append(out, discoveredRoot{root: root, language: primary})
	for _, d := range siblings {
		if !slices.ContainsFunc(out, func(e discoveredRoot) bool {
			return e.root == d.root && e.language == d.language
		}) {
			out = append(out, d)
		}
	}
	return out
}
