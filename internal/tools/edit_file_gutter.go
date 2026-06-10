package tools

// edit_file_gutter.go — forgiveness for old_strings that accidentally include
// the read_file/read_symbol display line-number gutter ("<n>\t" at line start).
// The gutter is display-only and agents are told to strip it, but a client that
// pastes a guttered block verbatim would otherwise burn a failed-edit
// round-trip. The literal match always runs first — this fallback fires only
// after a miss, and only on an unambiguous gutter shape (every line prefixed,
// numbers consecutive), so genuine content that happens to start with
// digits+tab (a TSV row) is never rewritten. Single-line strings are never
// auto-stripped for the same reason; they get a hint via notFoundError instead.

import (
	"strings"
)

// parseGutterPrefix splits one line into its gutter value and the remainder.
// A gutter prefix is optional leading spaces (the gutter is right-aligned),
// one or more digits, then exactly one tab.
func parseGutterPrefix(line string) (n int, rest string, ok bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	start := i
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		n = n*10 + int(line[i]-'0')
		i++
	}
	if i == start || i >= len(line) || line[i] != '\t' {
		return 0, "", false
	}
	return n, line[i+1:], true
}

// stripLineGutter removes the display line-number gutter from every line of s.
// It strips only when the shape is unambiguous: at least two gutter-bearing
// lines, EVERY line carries the prefix, and the numbers increase by exactly 1
// — the shape of a block copied verbatim from guttered read output. A final
// empty piece (the trailing newline of a copied block) is permitted and
// preserved. Returns (s, false) when the shape doesn't hold.
func stripLineGutter(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	// Permit (and preserve) the empty piece a trailing newline produces.
	body := lines
	if len(body) > 1 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}
	if len(body) < 2 {
		return s, false
	}
	stripped := make([]string, len(lines))
	prev := 0
	for i, line := range body {
		n, rest, ok := parseGutterPrefix(line)
		if !ok || (i > 0 && n != prev+1) {
			return s, false
		}
		prev = n
		stripped[i] = rest
	}
	// Carry the preserved trailing empty piece (if any) through unchanged.
	for i := len(body); i < len(lines); i++ {
		stripped[i] = lines[i]
	}
	return strings.Join(stripped, "\n"), true
}

// looksGuttered reports whether every line of s (ignoring a trailing empty
// piece) carries a gutter-shaped prefix, with no sequence requirement. Used
// only for the not-found error hint, where a false positive costs one
// suggestion line, never a content change.
func looksGuttered(s string) bool {
	lines := strings.Split(s, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return false
	}
	for _, line := range lines {
		if _, _, ok := parseGutterPrefix(line); !ok {
			return false
		}
	}
	return true
}

// gutterStrippedNote is appended to edit responses when forgiveness fired, so
// the agent learns to omit the prefix rather than relying on the fallback.
const gutterStrippedNote = "old_string included the display-only line-number gutter from read_file — " +
	"stripped automatically before matching; omit the \"<n>\\t\" prefix in future edits"

// resolveStrMatch normalises a str_replace edit against content and counts its
// occurrences, retrying once with the line-number gutter stripped when the
// literal form misses. The stripped form is used only when it resolves cleanly
// (exactly once, or at least once under replace_all); new_string is stripped
// too when it carries the same unambiguous gutter shape, so a verbatim
// guttered paste cannot inject gutter text into the file. The literal match
// always wins when it succeeds.
func resolveStrMatch(content string, edit strEdit) (oldStr, newStr string, count int, gutterStripped bool) {
	oldStr = matchLineEndings(edit.OldStr, content)
	newStr = matchLineEndings(edit.NewStr, content)
	count = strings.Count(content, oldStr)
	if count > 0 {
		return oldStr, newStr, count, false
	}
	strippedOld, ok := stripLineGutter(oldStr)
	if !ok {
		return oldStr, newStr, 0, false
	}
	c := strings.Count(content, strippedOld)
	if c == 0 || (!edit.ReplaceAll && c != 1) {
		return oldStr, newStr, 0, false
	}
	if strippedNew, ok := stripLineGutter(newStr); ok {
		newStr = strippedNew
	}
	return strippedOld, newStr, c, true
}
