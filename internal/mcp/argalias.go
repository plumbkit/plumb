package mcp

import "strings"

// aliasTarget is one canonical parameter an alias key may stand in for. Most
// entries are a plain rename (xf == noTransform). A transform additionally
// rewrites the VALUE as the rename is applied, for the few aliases whose name
// states a semantics rather than just naming a parameter:
//   - invertBool: a value-inverting flag (-i / ignore_case → case_sensitive
//     with the value negated);
//   - constTrue: a constant flag (preview → dry_run:true, regex →
//     use_regex:true) — the value is forced, and the candidate only FITS a
//     truthy value, so `preview:false` is left for validation rather than
//     silently flipped;
//   - wrapScalar: a scalar wrapped into a one-element array when the
//     canonical is array-typed (kind → kinds, path → uris).
//
// Transforms are allowed only for intent-explicit flags: the caller explicitly
// named the flag, so the transform IS the semantics the name states — applying
// it honours the caller's stated intent rather than guessing it.
// (safetyCriticalParams keeps FUZZY typo promotion away from the guarded
// canonical names; curated transforms are deliberate, not fuzzy.)
type aliasTarget struct {
	name string
	xf   transform
}

// Plain / transform-carrying aliasTarget constructors, keeping the table rows
// one-liners.
func plain(name string) aliasTarget    { return aliasTarget{name: name} }
func invert(name string) aliasTarget   { return aliasTarget{name: name, xf: invertBool} }
func constant(name string) aliasTarget { return aliasTarget{name: name, xf: constTrue} }
func wrap(name string) aliasTarget     { return aliasTarget{name: name, xf: wrapScalar} }

// paramAliases maps a normalised parameter name (see normaliseKey) to the
// canonical name(s) it may stand in for, most-preferred first. The resolver
// only applies a mapping when the canonical name is an actual parameter of the
// tool being called and isn't already set — so the same alias is safe across
// tools with different shapes (e.g. "path" maps to "file_path" on read_file,
// but stays canonical on find_files where "path" is the real parameter).
//
// plumb's canonical names follow Claude Code's native tools (file_path,
// old_string, new_string for file-content tools; path/pattern for search/dir
// tools); this table lets other agents — and plumb's earlier conventions —
// reach the same parameters without a failed call.
//
// Candidate order is most-preferred-first; the first one that is a real,
// unset parameter of the called tool whose transform (if any) fits the given
// value wins, so a single alias serves tools with different shapes (e.g.
// "path" → uri on get_definition, and stays canonical on search_in_files where
// "path" is the real parameter). New entries are
// empirically driven (the parameter names agents actually send, mined from
// the stats DB) and must be unambiguous — never a semantic flip
// (include≠exclude) or a safety-critical guess (no git subcommand/confirm).
// Value transforms (aliasTarget.xf) are the narrow, deliberate exception:
// permitted only where the alias name itself states the semantics (an
// intent-explicit flag like -i or preview).
var paramAliases = map[string][]aliasTarget{
	// File / directory location. Note keys are matched post-normalisation (see
	// normaliseKey), so "filepath" already covers a literal `file_path` argument —
	// hence no separate "file_path" entry. "uri" is the reciprocal that lets the
	// LSP tools' `uri` cross-accept onto the file/dir tools' file_path/path/root
	// (read_file({uri: …}) previously errored because no "uri" key existed). The
	// trailing wrap candidate lets a scalar stand in for the array-typed `uris`
	// (diagnostics) wherever no scalar location parameter exists.
	"path":      {plain("file_path"), plain("uri"), wrap("uris"), plain("uris")},
	"uri":       {plain("file_path"), plain("path")},
	"filepath":  {plain("file_path"), plain("path"), plain("uri")},
	"filename":  {plain("file_path"), plain("uri")},
	"file":      {plain("file_path"), plain("path"), plain("uri"), wrap("uris"), plain("uris")},
	"filepaths": {plain("paths"), plain("file_path")},
	"dir":       {plain("path")},
	"directory": {plain("path")},
	"folder":    {plain("path")},
	// "root" survives as a SOURCE only. It is the retired list_files spelling, so
	// a direct find_files({root: …}) is a call agents really make and it must
	// reach `path`. It is gone as a TARGET (it used to trail the path/uri/dir/
	// directory/folder rows): no shipped tool has declared a `root` parameter
	// since list_files was folded into find_files, and eligible() requires the
	// canonical to be a real parameter of the called tool, so those five
	// candidates could never fire. Carrying them was not free — every alias
	// target is dropped from the published `required` list by publishSchema, so
	// a future tool declaring `root` as required would have been advertised as
	// optional on the strength of five dead rows.
	"root": {plain("path")},
	// Edit content.
	"oldstr":  {plain("old_string")},
	"newstr":  {plain("new_string")},
	"find":    {plain("pattern")},
	"replace": {plain("replacement")},
	// Search / symbol query. "regex" is two intents sharing a name: a string
	// value is the pattern itself (the long-standing behaviour); a truthy bool
	// is the intent-explicit "treat the pattern as a regex" flag, so only then
	// does it rewrite to use_regex:true.
	"regex":   {constant("use_regex"), plain("pattern")},
	"query":   {plain("pattern"), plain("name")},
	"pattern": {plain("query")},
	"name":    {plain("query"), plain("symbol_name")},
	"newname": {plain("name")},
	"symbol":  {plain("name"), plain("symbol_name"), plain("query")},
	// read_symbol / read_memory call it `name`, while get_definition and
	// find_references call the same thing `symbol_name` — so an agent moving
	// between them sends the other tool's spelling. 40 read_symbol({symbol_name})
	// rejections in the stats DB, the single largest unknown-parameter group.
	"symbolname":  {plain("name"), plain("symbol_name")},
	"namepath":    {plain("name")},
	"memoryname":  {plain("name")},
	"isregex":     {plain("use_regex")},
	"filepattern": {plain("glob")},
	// A file filter agents spell `include`, against tools that call it `glob`.
	// It is NOT a semantic flip: the canonical it reaches is the INCLUDE filter.
	// Left unmapped it was worse than a rejection — `exclude` is the nearest
	// declared name by edit distance, so the "did you mean" hint pointed at the
	// OPPOSITE filter (see antonymStems).
	"include": {plain("glob")},
	// Case sensitivity: grep-style value-inverting flags (-i normalises to "i").
	"i":          {invert("case_sensitive")},
	"ignorecase": {invert("case_sensitive")},
	// Preview / dry-run.
	"preview": {constant("dry_run")},
	// Array-typed filters: wrap the scalar into a one-element array. The plain
	// fallback covers a value that is already an array.
	"kind": {wrap("kinds"), plain("kinds")},
	// Move / copy.
	"source":      {plain("from")},
	"destination": {plain("to")},
	// File content (write_file / write_memory).
	"text":     {plain("content")},
	"contents": {plain("content")},
	"body":     {plain("content")},
	"data":     {plain("content")},
	// Edit batches (edit_file).
	"changes":      {plain("edits")},
	"replacements": {plain("edits")},
	// Read window / result caps. The n-lines synonyms serve every capped tool
	// (read_file's limit, search's max_results/max_matches, workspace_sessions'
	// recent_limit — first eligible wins); "limit" itself crosses only to tools
	// WITHOUT a limit parameter (eligibility skips it where limit is declared),
	// where the same cap goes by a different name.
	"start":     {plain("start_line")},
	"end":       {plain("end_line")},
	"linestart": {plain("start_line")},
	"lineend":   {plain("end_line")},
	"nlines":    {plain("limit"), plain("max_results"), plain("recent_limit"), plain("max_matches")},
	"numlines":  {plain("limit"), plain("max_results"), plain("recent_limit"), plain("max_matches")},
	"maxlines":  {plain("limit"), plain("max_results"), plain("recent_limit"), plain("max_matches")},
	"linecount": {plain("limit"), plain("max_results"), plain("recent_limit"), plain("max_matches")},
	"limit":     {plain("max_results"), plain("max_matches"), plain("recent_limit")},
	// The reciprocal of "limit". workspace_search caps with `limit` while the
	// search tools cap with `max_results`, so the crossing is needed both ways.
	"maxresults": {plain("limit"), plain("max_matches"), plain("recent_limit")},
	"maxmatches": {plain("max_matches"), plain("max_results")},
	"maxcount":   {plain("max_matches"), plain("max_results")},
	// NOT mapped: column/col → character. It is a real spelling agents send (3
	// calls in the stats DB), but `character` is REQUIRED on explain_symbol and
	// type_hierarchy, and publishSchema drops every alias target from the
	// published `required` list so a pre-validating host cannot reject the alias
	// before it reaches plumb. Three calls does not buy weakening the
	// required-ness signal on two other tools. TestAliasTargetSet_ExactMembership
	// is what forces this trade to be made deliberately.
	// Traversal depth: find_files calls it max_depth, the topology tools call it
	// depth — both directions.
	"depth":    {plain("max_depth")},
	"maxdepth": {plain("depth")},
	// Find/filter.
	"ext": {plain("extension")},
	// Hidden files.
	"hidden": {plain("include_hidden")},
	// Directory listing order.
	"sort":    {plain("sort_by")},
	"orderby": {plain("sort_by")},
	// Tasks.
	"task": {plain("slot")},
	// Git.
	"msg":           {plain("message")},
	"commitmessage": {plain("message")},
	"repository":    {plain("repo")},
	"subcmd":        {plain("subcommand")},
	"op":            {plain("subcommand")},
	"command":       {plain("subcommand")},
	// Workspace pin.
	"workspacepath": {plain("workspace")},
}

// Why an unknown parameter is REJECTED rather than warned-about and dropped.
//
// The question was reopened against data, not argument: 296 unknown-parameter
// rejections in the stats DB. Most were spellings the curated table simply did
// not carry, and those are now rows above — a rewrite keeps the caller's VALUE,
// so it costs nothing.
//
// The remainder are the case for keeping the rejection, and they are not typos.
// Agents send `edits[].new_string_2`, `new_string_unused`, `old_string_offset` —
// invented parameters, reaching for a shape edit_file does not have. Dropping
// `new_string_2` with a warning would apply the first replacement, silently skip
// the second, and report success: a partial edit that looks like a whole one.
// No warning string prevents that, because by the time it is read the write has
// happened.
//
// The earlier statement of this rule was that dropping masks a mistyped SAFETY
// flag. That much is true but it is the weaker half: a dropped `dirty_ok` or
// `confirm` fails CLOSED — the operation is refused, which is safe if annoying.
// The real cost is a dropped VALUE, where absence is not a safer default but a
// different, silently smaller change.
//
// So the two paths stay as they are: rewrite when the value can be carried to a
// canonical, reject when it cannot. What was actually missing was neither — it
// was the quality of the rejection, and closest() had a hole (see antonymStems).

// safetyCriticalParams names canonical parameters a fuzzy (edit-distance) match
// must never auto-correct TO: a wrong guess here flips a side-effect or defeats a
// guard, so an ambiguous typo is surfaced as a rejection ("did you mean") rather
// than silently applied. The curated paramAliases table and the case/separator-
// insensitive match are still allowed for these names — only edit-distance
// guessing is gated, because those two are exact, not approximate. The curated
// value transforms (aliasTarget.xf) may likewise target these names: they are
// deliberate, intent-explicit rewrites, not guesses.
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
// returns the zero aliasTarget and false. It tries the curated alias table
// first (skipping candidates whose transform does not fit the given value, and
// candidates another key in the same pass already claimed), then a
// case/separator-insensitive match against the level's declared parameters. It
// never guesses by edit distance — that approximate path lives in the
// separately gated fuzzyCanonical (rewriteObject's second pass), so the curated
// resolution here stays exact.
//
// claimed carries the canonicals already taken by earlier keys of the same
// call, so two aliases of one canonical (write_file given both `text` and
// `body`; read_file given both `path` and `file`) can never both rewrite to it
// — the second key tries its remaining candidates and, failing those, is left
// for validation's explicit "unknown parameter" rejection. Silently dropping
// one of two supplied values would be far worse than the error.
func canonicalFor(key string, sh *shape, obj map[string]any, claimed map[string]bool) (aliasTarget, bool) {
	nk := normaliseKey(key)
	for _, cand := range paramAliases[nk] {
		if eligible(cand.name, sh, obj) && !claimed[cand.name] && cand.xf.fits(sh, cand.name, obj[key]) {
			return cand, true
		}
	}
	match, count := "", 0
	for _, p := range sh.order {
		if normaliseKey(p) == nk {
			match, count = p, count+1
		}
	}
	if count == 1 && eligible(match, sh, obj) && !claimed[match] {
		return aliasTarget{name: match}, true
	}
	return aliasTarget{}, false
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

// antonymStems are meaning-inverting word pairs. A suggestion that differs from
// the typed key by one of these is not a typo correction — it is the OPPOSITE
// parameter, and following it produces a silently wrong result rather than an
// error.
//
// This is the "never a semantic flip (include≠exclude)" rule the alias table
// states, applied where it was missing. The rule was enforced on the two paths
// that REWRITE a call (the curated table is hand-audited; fuzzyCanonical is
// gated by safetyCriticalParams), but not on the path that ADVISES one. Measured
// on the stats DB: `search_in_files({include: "*.go"})` — 12 calls — was rejected
// with `did you mean "exclude"?`, because `exclude` is the nearest declared name
// by edit distance. An agent that took the advice would have searched with the
// inverse filter and got a confidently wrong answer.
var antonymStems = [][2]string{
	{"include", "exclude"},
	{"before", "after"},
	{"start", "end"},
	{"first", "last"},
	{"min", "max"},
	{"enable", "disable"},
	{"enabled", "disabled"},
	{"allow", "deny"},
	{"old", "new"},
	{"add", "remove"},
	{"in", "out"},
	{"asc", "desc"},
}

// nameTokens splits a parameter name into lowercase words, on separators and
// camelCase boundaries: start_line → [start line], includeHidden → [include
// hidden].
//
// Antonyms are compared as WHOLE TOKENS, never as substrings. Substring matching
// reads "append" as containing "end", "threshold" as containing "old" and
// "line" as containing "in", so it would suppress unrelated, useful suggestions
// — and the short pairs (in/out, min/max) are the ones a parameter vocabulary
// actually uses, so dropping them instead is not an option either.
func nameTokens(s string) map[string]bool {
	out := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out[strings.ToLower(cur.String())] = true
			cur.Reset()
		}
	}
	var prev rune
	for i, r := range s {
		switch {
		case r == '_' || r == '-' || r == ' ' || r == '.':
			flush()
		case i > 0 && r >= 'A' && r <= 'Z' && (prev < 'A' || prev > 'Z'):
			flush()
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
		prev = r
	}
	flush()
	return out
}

// invertsMeaning reports whether suggesting cand for key would swap a meaning
// rather than fix a spelling: one name carries a stem as a whole token and the
// other carries its opposite. A name carrying BOTH sides of a pair is not an
// inversion of anything.
func invertsMeaning(key, cand string) bool {
	k, c := nameTokens(key), nameTokens(cand)
	for _, pair := range antonymStems {
		a, b := pair[0], pair[1]
		if k[a] && k[b] || c[a] && c[b] {
			continue
		}
		if (k[a] && c[b]) || (k[b] && c[a]) {
			return true
		}
	}
	return false
}

// closest returns the candidate most similar to key (case-insensitive
// Levenshtein) when it is close enough to be a plausible typo — used only for
// the "did you mean" hint on a rejected unknown parameter.
//
// A candidate whose meaning INVERTS the key's is skipped rather than offered,
// even when it is the nearest by edit distance: a hint is advice an agent acts
// on, and advice to use the opposite filter is worse than no advice at all. The
// next-nearest non-inverting candidate is offered instead, or nothing.
func closest(key string, candidates []string) string {
	lowerKey := strings.ToLower(key)
	best, bestDist := "", -1
	for _, c := range candidates {
		if invertsMeaning(key, c) {
			continue
		}
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

// transform identifies the optional value rewrite an aliasTarget applies as
// the rename happens. See aliasTarget for the policy (intent-explicit flags
// only).
type transform int

const (
	noTransform transform = iota
	invertBool            // bool → its negation (value-inverting flag: -i → case_sensitive)
	constTrue             // force true; fits only a truthy value (constant flag: preview → dry_run:true)
	wrapScalar            // scalar → one-element array when the canonical is array-typed
)

// fits reports whether the transform suits the value the caller gave AND the
// canonical it would write to. A candidate whose transform does not fit is
// skipped, so the next candidate (or validation's "did you mean") sees the key
// — a transform never fires on a value it cannot honour.
func (t transform) fits(sh *shape, to string, v any) bool {
	switch t {
	case invertBool:
		_, ok := boolValue(v)
		return ok
	case constTrue:
		b, ok := boolValue(v)
		return ok && b
	case wrapScalar:
		// Only a scalar needs wrapping, and only into an array-typed canonical:
		// an already-array value falls through to the plain-rename candidate for
		// the same canonical, and a non-array target would report "wrapped in a
		// single-element array" for what apply() leaves as a plain rename.
		if sh.types[to] != "array" {
			return false
		}
		_, isArray := v.([]any)
		return !isArray
	}
	return true
}

// apply rewrites the value for the renamed key. sh/to identify the canonical
// parameter (wrapScalar consults its declared type); by the time apply runs a
// bool transform always fits.
func (t transform) apply(sh *shape, to string, v any) any {
	switch t {
	case invertBool:
		b, _ := boolValue(v)
		return !b
	case constTrue:
		return true
	case wrapScalar:
		if sh.types[to] == "array" {
			if _, isArray := v.([]any); !isArray {
				return []any{v}
			}
		}
	}
	return v
}

// describe is the short parenthetical added to the alias notice when a
// transform fired ("" for a plain rename).
func (t transform) describe() string {
	switch t {
	case invertBool:
		return "inverted value"
	case constTrue:
		return "forced true"
	case wrapScalar:
		return "wrapped in a single-element array"
	}
	return ""
}

// boolValue reads a JSON bool, or the strings "true"/"false" (any case), as a
// bool. ok is false for anything else.
func boolValue(v any) (b, ok bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(t) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
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
