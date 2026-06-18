package mcp

import "strings"

// paramAliases maps a normalised parameter name (see normaliseKey) to the
// canonical name(s) it may stand in for, most-preferred first. The resolver
// only applies a mapping when the canonical name is an actual parameter of the
// tool being called and isn't already set — so the same alias is safe across
// tools with different shapes (e.g. "path" maps to "file_path" on read_file,
// but stays canonical on list_directory where "path" is the real parameter).
//
// plumb's canonical names follow Claude Code's native tools (file_path,
// old_string, new_string for file-content tools; path/pattern for search/dir
// tools); this table lets other agents — and plumb's earlier conventions —
// reach the same parameters without a failed call.
//
// Candidate order is most-preferred-first; the first one that is a real,
// unset parameter of the called tool wins, so a single alias serves tools with
// different shapes (e.g. "path" → uri on get_definition, → root on list_files,
// and stays canonical on search_in_files where "path" is the real parameter).
// New entries are empirically driven (the parameter names agents actually send,
// mined from the stats DB) and must be unambiguous — never a semantic flip
// (include≠exclude) or a safety-critical guess (no git subcommand/confirm).
var paramAliases = map[string][]string{
	// File / directory location.
	"path":      {"file_path", "uri", "root"},
	"filepath":  {"file_path", "path", "uri"},
	"filename":  {"file_path", "uri"},
	"file":      {"file_path", "path", "uri"},
	"filepaths": {"paths", "file_path"},
	"dir":       {"path", "root"},
	"directory": {"path", "root"},
	"folder":    {"path", "root"},
	"root":      {"path"},
	// Edit content.
	"oldstr":  {"old_string"},
	"newstr":  {"new_string"},
	"find":    {"pattern"},
	"replace": {"replacement"},
	// Search / symbol query.
	"regex":       {"pattern"},
	"query":       {"pattern", "name"},
	"pattern":     {"query"},
	"name":        {"query", "symbol_name"},
	"newname":     {"name"},
	"symbol":      {"name", "symbol_name", "query"},
	"isregex":     {"use_regex"},
	"filepattern": {"glob"},
	// Move / copy.
	"source":      {"from"},
	"destination": {"to"},
	// Workspace pin.
	"workspacepath": {"workspace"},
}

// aliasNotice formats the leading note prepended to a tool result when one or
// more parameter aliases were applied, nudging the caller toward the canonical
// names without failing the call.
func aliasNotice(warnings []string) string {
	return "note: " + strings.Join(warnings, "; ") + " — prefer the tool's documented parameter names.\n\n"
}

// canonicalFor resolves an unknown key to a canonical parameter of sh, or
// returns ("", false). It tries the curated alias table first, then a
// case/separator-insensitive match against the level's declared parameters. It
// never guesses by edit distance — a fuzzy near-match is surfaced only as a
// suggestion in the rejection error, never silently applied (especially for
// side-effecting tools).
func canonicalFor(key string, sh *shape, obj map[string]any) (string, bool) {
	nk := normaliseKey(key)
	for _, canon := range paramAliases[nk] {
		if eligible(canon, sh, obj) {
			return canon, true
		}
	}
	match, count := "", 0
	for _, p := range sh.order {
		if normaliseKey(p) == nk {
			match, count = p, count+1
		}
	}
	if count == 1 && eligible(match, sh, obj) {
		return match, true
	}
	return "", false
}

// eligible reports whether canon is a real parameter at this level that the
// caller hasn't already provided — the safety condition for applying an alias.
func eligible(canon string, sh *shape, obj map[string]any) bool {
	if _, ok := sh.props[canon]; !ok {
		return false
	}
	_, present := obj[canon]
	return !present
}

// normaliseKey folds case and drops separators so camelCase, snake_case, and
// kebab-case variants of the same name collapse together
// (startLine / start_line / start-line → "startline").
func normaliseKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '_' || r == '-' || r == ' ':
			continue
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// closest returns the candidate most similar to key (case-insensitive
// Levenshtein) when it is close enough to be a plausible typo — used only for
// the "did you mean" hint on a rejected unknown parameter.
func closest(key string, candidates []string) string {
	lowerKey := strings.ToLower(key)
	best, bestDist := "", -1
	for _, c := range candidates {
		d := levenshtein(lowerKey, strings.ToLower(c))
		if bestDist == -1 || d < bestDist {
			best, bestDist = c, d
		}
	}
	threshold := max(len(key)/2, 2)
	if bestDist >= 0 && bestDist <= threshold {
		return best
	}
	return ""
}

// levenshtein is the classic two-row edit distance.
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
