package memory

import "slices"

// CodeRef identifies a code entity by rebuild-proof fields; zero-value fields
// simply do not participate in matching. It is the topology ↔ memory join
// contract: memory may reference code entities by stable fields, and the join
// happens in plain Go at the tool layer — memory never imports topology,
// topology never depends on memory, and topology row IDs are never stored
// (the topology index is rebuildable, so row IDs change on reindex).
type CodeRef struct {
	Kind       string // function, method, type, … (informational)
	File       string // workspace-relative slash path
	SymbolName string // symbol name as the extractor records it
	Package    string // optional package/module (informational)
}

// RefHit is one memory related to a set of CodeRefs, with the reason it
// matched. Frontmatter-level only — never a memory body.
type RefHit struct {
	Name        string
	Description string
	Confidence  Confidence
	Why         string
}

// MemoriesForRefs returns up to max memories related to the given code refs.
// A memory matches when a provenance source_symbol equals a ref's symbol
// name, when one of its paths: globs matches a ref's file, or when a
// provenance source_path equals a ref's file. User-authored memories claim
// slots before generated ones — the same rule as the hint block, so machine
// summaries never crowd out hand-written notes. Pure and deterministic over
// the caller-supplied list (memory.List order).
func MemoriesForRefs(mems []Memory, refs []CodeRef, max int) []RefHit {
	if max <= 0 || len(mems) == 0 || len(refs) == 0 {
		return nil
	}
	var user, generated []RefHit
	for _, m := range mems {
		why, ok := refMatch(m, refs)
		if !ok {
			continue
		}
		hit := RefHit{Name: m.Name, Description: m.Description, Confidence: m.Confidence, Why: why}
		if m.UserAuthored() {
			user = append(user, hit)
		} else {
			generated = append(generated, hit)
		}
	}
	hits := append(user, generated...)
	if len(hits) > max {
		hits = hits[:max]
	}
	return hits
}

// refMatch reports whether m relates to any of refs, and why. Symbol matches
// outrank path matches in the reported reason because they are the stronger
// signal (an exact identifier, not a glob).
func refMatch(m Memory, refs []CodeRef) (string, bool) {
	for _, r := range refs {
		if r.SymbolName != "" && slices.Contains(m.SourceSymbols, r.SymbolName) {
			return "references symbol " + r.SymbolName, true
		}
	}
	for _, r := range refs {
		if r.File == "" {
			continue
		}
		if m.MatchesPath(r.File) {
			return "paths glob matches " + r.File, true
		}
		if slices.Contains(m.SourcePaths, r.File) {
			return "session provenance touched " + r.File, true
		}
	}
	return "", false
}
