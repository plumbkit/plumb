package tools

import (
	"regexp"
	"strings"
)

// The literal-mode regex nudge, shared by search_in_files, read_file's pattern
// mode, and find_replace.
//
// Every one of those tools defaults to use_regex:false and runs the pattern
// through regexp.QuoteMeta, so "foo\.bar" or "a|b" is matched as literal text.
// The result is a clean, confident wrong answer — the reason this hint exists.
//
// Two tiers, because the hint now fires on a NON-EMPTY result too and a single
// broad detector would then flag ordinary literal searches constantly:
//
//   - STRONG syntax is rare in real source text but standard in a regex, so it
//     is worth flagging whether or not the search matched something.
//   - WEAK syntax — character classes, groups, counted quantifiers, a trailing
//     `$` — is far too common in ordinary code ("args[0]", "func main()",
//     "${VAR}") to flag on a successful search. It is reported only on a
//     ZERO-match result, where the alternative reading is a false "these don't
//     exist" and there is no noise cost.
//
// Deliberately never flagged in either tier: a bare `.`, `+`, `?`, or `*`.

// strongRegexEscapes are backslash escapes that only mean something in a regex.
// `\n`, `\t`, `\\` and `\"` are excluded — they are ordinary string-literal
// escapes that appear constantly in code being searched literally.
var strongRegexEscapes = []string{`\.`, `\d`, `\D`, `\w`, `\W`, `\s`, `\S`, `\b`, `\B`}

// weakQuantifier matches a counted quantifier such as {2} or {1,4}. Requires
// digits so an ordinary brace in code ("{}", "${VAR}") does not qualify.
var weakQuantifier = regexp.MustCompile(`\{\d+(,\d*)?\}`)

// strongRegexSyntax names the first unambiguous regex construct in pattern, or
// "" if there is none.
func strongRegexSyntax(pattern string) string {
	switch {
	case strings.Contains(pattern, "|"):
		return "| alternation"
	case strings.Contains(pattern, ".*"), strings.Contains(pattern, ".+"):
		return ".* wildcard"
	case strings.HasPrefix(pattern, "^"):
		return "^ anchor"
	}
	for _, esc := range strongRegexEscapes {
		if strings.Contains(pattern, esc) {
			return esc + " escape"
		}
	}
	return ""
}

// weakRegexSyntax names the first regex construct that is also common in
// ordinary code, or "" if there is none.
func weakRegexSyntax(pattern string) string {
	if weakQuantifier.MatchString(pattern) {
		return "{n,m} quantifier"
	}
	if i := strings.IndexByte(pattern, '['); i >= 0 && strings.IndexByte(pattern[i:], ']') > 1 {
		return "[...] character class"
	}
	if i := strings.IndexByte(pattern, '('); i >= 0 && strings.IndexByte(pattern[i:], ')') > 1 {
		return "(...) group"
	}
	if len(pattern) > 1 && strings.HasSuffix(pattern, "$") {
		return "$ anchor"
	}
	return ""
}

// literalRegexHint returns the one-line nudge, or "" when there is nothing to
// say. matched reports whether the call found anything: a zero-match result
// widens the detector to the weak tier (see the file comment).
func literalRegexHint(pattern string, useRegex, matched bool) string {
	if useRegex {
		return ""
	}
	syntax := strongRegexSyntax(pattern)
	if syntax == "" && !matched {
		syntax = weakRegexSyntax(pattern)
	}
	if syntax == "" {
		return ""
	}
	return "\nNote: the pattern contains regex syntax (" + syntax +
		") but use_regex is false, so it was matched literally. " +
		"Pass use_regex: true to treat it as a pattern."
}
