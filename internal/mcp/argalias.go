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
	// File / directory location. Note keys are matched post-normalisation (see
	// normaliseKey), so "filepath" already covers a literal `file_path` argument —
	// hence no separate "file_path" entry. "uri" is the reciprocal that lets the
	// LSP tools' `uri` cross-accept onto the file/dir tools' file_path/path/root
	// (read_file({uri: …}) previously errored because no "uri" key existed).
	"path":      {"file_path", "uri", "root"},
	"uri":       {"file_path", "path", "root"},
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
	// File content (write_file / write_memory).
	"text":     {"content"},
	"contents": {"content"},
	"body":     {"content"},
	// Read window.
	"start": {"start_line"},
	"end":   {"end_line"},
	// Tasks.
	"task": {"slot"},
	// Git.
	"msg":           {"message"},
	"commitmessage": {"message"},
	"repository":    {"repo"},
	"subcmd":        {"subcommand"},
	// Workspace pin.
	"workspacepath": {"workspace"},
}

// safetyCriticalParams names canonical parameters a fuzzy (edit-distance) match
// must never auto-correct TO: a wrong guess here flips a side-effect or defeats a
// guard, so an ambiguous typo is surfaced as a rejection ("did you mean") rather
// than silently applied. The curated paramAliases table and the case/separator-
// insensitive match are still allowed for these names — only edit-distance
// guessing is gated, because those two are exact, not approximate.
var safetyCriticalParams = map[string]bool{
	"confirm":           true,
	"use_regex":         true,
	"replace_all":       true,
	"allow_dir":         true,
	"dirty_ok":          true,
	"overwrite_changed": true,
	"reconcile":         true,
	"expected_mtime":    true,
	"expected_sha":      true,
	"subcommand":        true,
	"force":             true,
}

// fuzzyCanonical promotes a high-confidence single-character typo of a declared
// parameter to an auto-rewrite: a UNIQUE candidate at edit distance 1, currently
// unset, not safety-critical, and a typed key long enough (≥4 runes) that a
// distance-1 match is meaningful rather than coincidental. Anything looser — a
// tie, distance ≥2, a short key, or a guarded target — returns false, so the
// caller leaves the key for validation's "did you mean" rejection. This is the
// approximate path canonicalFor deliberately refuses; it stays separate so the
// curated alias resolution remains exact.
func fuzzyCanonical(key string, sh *shape, obj map[string]any) (string, bool) {
	if len([]rune(key)) < 4 {
		return "", false
	}
	lowerKey := strings.ToLower(key)
	best, bestDist, ties := "", -1, 0
	for _, p := range sh.order {
		d := levenshtein(lowerKey, strings.ToLower(p))
		switch {
		case bestDist == -1 || d < bestDist:
			best, bestDist, ties = p, d, 1
		case d == bestDist:
			ties++
		}
	}
	if bestDist != 1 || ties != 1 {
		return "", false
	}
	if safetyCriticalParams[best] {
		return "", false
	}
	if !eligible(best, sh, obj) {
		return "", false
	}
	return best, true
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
// never guesses by edit distance — that approximate path lives in the separately
// gated fuzzyCanonical (rewriteObject's second pass), so the curated resolution
// here stays exact.
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
