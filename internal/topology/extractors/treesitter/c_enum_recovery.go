package treesitter

// This file holds a TEMPORARY workaround for an upstream gotreesitter parse
// defect, kept separate from the extractor proper so that retiring it is a
// single file deletion rather than surgery on c.go. See
// TestC_EnumRecovery_TripwireForUpstreamFix, which fails once the defect is
// fixed and tells whoever hits it to delete this file.

import (
	"strings"

	tsg "github.com/odvcencio/gotreesitter"

	"github.com/plumbkit/plumb/internal/topology"
)

// recoverEnumerators emits the enumerators the grammar dropped on the floor.
//
// UPSTREAM DEFECT, measured on gotreesitter v0.48.1: an enum whose body holds
// EXACTLY THREE enumerators and no trailing comma parses with a zero-width
// MISSING token before the closing brace, and the THIRD enumerator never appears
// in the tree. One, two, four and five enumerators are all clean, as is the same
// enum written with a trailing comma — so this is not a size limit but a
// specific GLR defect.
//
// It cannot be left alone. Three-member enums are ordinary C, and the failure is
// silent: `enum Colour { RED, GREEN, BLUE }` yielded RED and GREEN and simply
// lost BLUE, which is the definition-shaped hole that makes a Map worse than no
// Map — an agent trusts the two it sees and never learns of the third.
//
// The recovery reads the enumerator names straight from the source text between
// the braces. That text is scanned, never split on raw commas: a comma separates
// enumerators only outside comments, character and string literals and
// bracketed regions, and a name is emitted only where an enumerator can legally
// begin. An INVENTED symbol is the same failure as a missing one with the sign
// flipped, and at confidence 1.0 it is worse — the corpus sweep that counts
// parse errors and span validity sees neither. Spans are computed from real byte
// offsets, so a recovered node is indistinguishable downstream from a parsed
// one.
//
// This mirrors recoverIUOBangs, the Swift workaround that was carried until
// gotreesitter fixed the underlying parse and then deleted.
// TestC_EnumRecovery_TripwireForUpstreamFix fails once upstream parses this
// cleanly, which is the signal to delete this function.
func (w *cWalk) recoverEnumerators(list *tsg.Node, parent int64, seen map[string]bool) {
	lo, hi := clampU32(list.StartByte()), clampU32(list.EndByte())
	if lo >= hi || hi > len(w.src) {
		return
	}
	base, body := enumBody(string(w.src[lo:hi]), lo)
	baseLine := line(list.StartPoint())
	for _, seg := range scanEnumerators(body) {
		text := body[seg[0]:seg[1]]
		name, rel := enumeratorName(text)
		if name == "" || seen[name] {
			continue
		}
		// Span the whole enumerator (`BLUE = 3`), not just its name, so a
		// recovered node is byte-identical to the parsed ones beside it —
		// consumers slice source with these spans and must not be able to tell
		// which came from the tree.
		start := base + seg[0] + rel
		end := base + seg[0] + enumeratorEnd(text, rel+len(name))
		if start < lo || end > hi || end <= start {
			continue
		}
		seen[name] = true
		idx := int64(len(w.nodes))
		// Lines come from the enumerator's own bytes, not the enclosing list's.
		// Giving a recovered node the whole enum's L1-5 while its parsed
		// siblings report L2-2 is exactly the tell this workaround promises not
		// to leave.
		startLine := baseLine + strings.Count(string(w.src[lo:start]), "\n")
		node := topology.Node{
			Kind:      topology.KindConstant,
			Name:      name,
			Qualified: w.qualify(parent, name),
			StartLine: startLine,
			EndLine:   startLine + strings.Count(string(w.src[start:end]), "\n"),
			Language:  w.langName,
			Path:      w.path,
			HasBytes:  true,
			StartByte: start,
			EndByte:   end,
		}
		w.nodes = append(w.nodes, node)
		w.link(parent, idx)
	}
}

// enumBody strips the braces from an enumerator list's source text, returning
// the interior and the absolute byte offset that interior begins at.
func enumBody(text string, at int) (int, string) {
	if i := strings.IndexByte(text, '{'); i >= 0 {
		text, at = text[i+1:], at+i+1
	}
	if j := strings.LastIndexByte(text, '}'); j >= 0 {
		text = text[:j]
	}
	return at, text
}

// scanEnumerators returns the top-level comma-separated segments of an
// enumerator-list body as [start,end) offsets into body.
//
// This is a scanner rather than strings.Split because a comma means "next
// enumerator" only at bracket depth zero and outside comments and literals.
// Splitting on raw commas fabricated symbols out of perfectly valid C:
// `enum E { A, /* red, green */ B, C }` produced a constant named `green`
// spanning "green */ B", and `A = MAX(x, y)` produced one named `y`.
func scanEnumerators(body string) [][2]int {
	var segs [][2]int
	depth, start := 0, 0
	for i := 0; i < len(body); {
		switch {
		case isCommentStart(body, i):
			i = skipComment(body, i)
		case body[i] == '\'' || body[i] == '"':
			i = skipQuoted(body, i)
		case body[i] == '(' || body[i] == '[' || body[i] == '{':
			depth++
			i++
		case body[i] == ')' || body[i] == ']' || body[i] == '}':
			if depth > 0 {
				depth--
			}
			i++
		case body[i] == ',' && depth == 0:
			segs = append(segs, [2]int{start, i})
			i++
			start = i
		default:
			i++
		}
	}
	if start < len(body) {
		segs = append(segs, [2]int{start, len(body)})
	}
	return segs
}

// enumeratorName returns the identifier an enumerator segment begins with and
// its offset within seg. It returns "" unless a C identifier is the FIRST thing
// in the segment once whitespace and comments are skipped, because that is the
// only position where an enumerator name can legally appear — anything else is
// evidence the segment is not an enumerator and must not become a symbol.
func enumeratorName(seg string) (string, int) {
	i := skipSpaceAndComments(seg, 0)
	if i >= len(seg) || !isCIdentStart(seg[i]) {
		return "", 0
	}
	j := i
	for j < len(seg) && (isCIdentStart(seg[j]) || isCDigit(seg[j])) {
		j++
	}
	return seg[i:j], i
}

// enumeratorEnd returns the offset just past the last source character of the
// enumerator, ignoring trailing whitespace and comments so `B /* note */` spans
// only "B" — the same span the grammar reports for a parsed sibling.
func enumeratorEnd(seg string, from int) int {
	last := from
	for i := from; i < len(seg); {
		switch {
		case isCSpace(seg[i]):
			i++
		case isCommentStart(seg, i):
			i = skipComment(seg, i)
		case seg[i] == '\'' || seg[i] == '"':
			i = skipQuoted(seg, i)
			last = i
		default:
			i++
			last = i
		}
	}
	return last
}

func skipSpaceAndComments(s string, i int) int {
	for i < len(s) {
		switch {
		case isCSpace(s[i]):
			i++
		case isCommentStart(s, i):
			i = skipComment(s, i)
		default:
			return i
		}
	}
	return i
}

func isCommentStart(s string, i int) bool {
	return i+1 < len(s) && s[i] == '/' && (s[i+1] == '/' || s[i+1] == '*')
}

// skipComment returns the offset just past the comment starting at i. An
// unterminated comment swallows the rest of the text, which is the conservative
// answer: no symbol is emitted from bytes we cannot classify.
func skipComment(s string, i int) int {
	if s[i+1] == '/' {
		if j := strings.IndexByte(s[i+2:], '\n'); j >= 0 {
			return i + 2 + j + 1
		}
		return len(s)
	}
	if j := strings.Index(s[i+2:], "*/"); j >= 0 {
		return i + 2 + j + 2
	}
	return len(s)
}

// skipQuoted returns the offset just past the character or string literal
// starting at i, honouring backslash escapes.
func skipQuoted(s string, i int) int {
	quote := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case quote:
			return j + 1
		}
	}
	return len(s)
}

func isCSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func isCIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isCDigit(b byte) bool { return b >= '0' && b <= '9' }

// hasMissingOrError reports whether n's subtree carries a parse defect. A
// MISSING node is zero-width and is NOT an ERROR, so a check for errors alone
// misses this entire class — the same blind spot that hid three Swift failures
// until the probe was taught to look for both.
func hasMissingOrError(n *tsg.Node) bool {
	if n.IsError() || n.IsMissing() {
		return true
	}
	for _, c := range n.Children() {
		if hasMissingOrError(c) {
			return true
		}
	}
	return false
}
