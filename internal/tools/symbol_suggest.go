package tools

import (
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// symbol_suggest.go builds the "did you mean?" fragment appended to the
// not-found message of the by-name symbol tools (read_symbol, get_definition,
// find_references, call_hierarchy). Suggestions live on the error path only:
// the exact-match contract of resolveSymbolsByName is untouched, so a
// body-returning tool never fuzzily substitutes a different symbol.

// maxSymbolSuggestions caps the "did you mean?" list, mirroring the
// three-candidate cap of the position-miss nearbySymbolHint.
const maxSymbolSuggestions = 3

// suggestSymbols returns up to maxSymbolSuggestions names from the document
// symbol tree that plausibly match a query that resolved to nothing. A
// candidate qualifies when one lowercased name contains the other (query
// "Watcher" inside "fsWatcher", or the reverse) or when the edit distance
// between the lowercased names is at most 2. Substring hits rank first, then
// ascending edit distance, then document order; duplicates and the query
// itself are dropped.
//
// Candidate names are the flattened tree's own names, except gopls-style flat
// Go methods ("(*Recv).Method"), which are offered in the dotted
// "Recv.Method" form resolveSymbolsByName accepts. A dotted query is compared
// whole — its typo distance to the dotted composite covers the
// Receiver.Method case without segment-level matching.
func suggestSymbols(syms []protocol.DocumentSymbol, name string) []string {
	type scored struct {
		name      string
		substring bool
		dist      int
	}
	lowerQuery := strings.ToLower(name)
	seen := make(map[string]bool)
	var candidates []scored
	for _, cand := range suggestionCandidates(syms) {
		if cand == name || seen[cand] {
			continue
		}
		seen[cand] = true
		lowerCand := strings.ToLower(cand)
		substring := strings.Contains(lowerCand, lowerQuery) || strings.Contains(lowerQuery, lowerCand)
		dist := levenshtein(lowerQuery, lowerCand)
		if !substring && dist > 2 {
			continue
		}
		candidates = append(candidates, scored{name: cand, substring: substring, dist: dist})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].substring != candidates[j].substring {
			return candidates[i].substring
		}
		return candidates[i].dist < candidates[j].dist
	})
	if len(candidates) == 0 {
		return nil
	}
	n := min(maxSymbolSuggestions, len(candidates))
	out := make([]string, 0, n)
	for _, c := range candidates[:n] {
		out = append(out, c.name)
	}
	return out
}

// suggestionCandidates flattens the symbol tree to the names an agent could
// pass back as a query, in document order.
func suggestionCandidates(syms []protocol.DocumentSymbol) []string {
	flat := flattenDocSymbols(syms)
	out := make([]string, 0, len(flat))
	for _, s := range flat {
		if recv, method, ok := goMethodReceiver(s.Name); ok {
			out = append(out, recv+"."+method)
			continue
		}
		out = append(out, s.Name)
	}
	return out
}

// didYouMean renders suggestions as a fragment appended to a not-found
// message (leading space; empty when there are no suggestions), pointing at
// find_symbol for anything beyond a near miss.
func didYouMean(suggestions []string) string {
	if len(suggestions) == 0 {
		return ""
	}
	quoted := make([]string, len(suggestions))
	for i, s := range suggestions {
		quoted[i] = "`" + s + "`"
	}
	return " Did you mean: " + strings.Join(quoted, ", ") + "? Use find_symbol for a fuzzy search."
}

// levenshtein is the classic two-row edit distance. Deliberately duplicated
// from internal/mcp (argalias.go): the layering contract forbids the
// application layer importing the transport layer, and a two-row loop is too
// small to justify a shared package.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}
