package treesitter

// This file holds a TEMPORARY workaround for an upstream gotreesitter parse
// defect, kept separate from the extractor proper so that retiring it is a
// single file deletion rather than surgery on c.go. See
// TestC_EnumRecovery_TripwireForUpstreamFix, which fails once the defect is
// fixed and tells whoever hits it to delete this file.

import (
	strings "strings"

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
// the braces, which is unambiguous for this construct: enumerators are
// comma-separated and each begins with an identifier. Spans are computed from
// real byte offsets, so a recovered node is indistinguishable downstream from a
// parsed one.
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
	body := string(w.src[lo:hi])
	offset := lo
	for _, seg := range strings.Split(strings.Trim(body, "{}"), ",") {
		name, rel := leadingIdentifier(seg)
		if name == "" {
			offset += len(seg) + 1
			continue
		}
		start := offset + strings.Index(body[offset-lo:], seg) + rel
		// Span the whole enumerator (`BLUE = 3`), not just its name, so a
		// recovered node is byte-identical to the parsed ones beside it —
		// consumers slice source with these spans and must not be able to tell
		// which came from the tree.
		end := start + len(strings.TrimRight(seg[rel:], " \t\r\n"))
		if seen[name] || start < lo || end > hi || end <= start {
			offset += len(seg) + 1
			continue
		}
		seen[name] = true
		idx := int64(len(w.nodes))
		node := topology.Node{
			Kind:      topology.KindConstant,
			Name:      name,
			Qualified: w.qualify(parent, name),
			StartLine: line(list.StartPoint()),
			EndLine:   line(list.EndPoint()),
			Language:  w.langName,
			Path:      w.path,
			HasBytes:  true,
			StartByte: start,
			EndByte:   end,
		}
		w.nodes = append(w.nodes, node)
		w.link(parent, idx)
		offset += len(seg) + 1
	}
}

// leadingIdentifier returns the first C identifier in seg and its offset within
// seg, ignoring leading whitespace and comments-free simple text. It stops at
// the first non-identifier character, so `BLUE = 3` yields "BLUE".
func leadingIdentifier(seg string) (string, int) {
	i := 0
	for i < len(seg) && isCSpace(seg[i]) {
		i++
	}
	start := i
	for i < len(seg) && (isCIdentStart(seg[i]) || (i > start && isCDigit(seg[i]))) {
		i++
	}
	if i == start {
		return "", 0
	}
	return seg[start:i], start
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
