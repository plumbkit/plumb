package tools

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_ack.go — the caller's acknowledgements of bodies it still holds (C15).
//
// have lists {symbol, content_sha256} pairs. When a body the pack would deliver
// is the one the caller says it holds, the pack says "unchanged, still held" in
// place of the body. The match is deliberately narrow:
//
//   - The hash must equal the content_sha256 of the exact body this pack would
//     deliver, the SHA-256 of that declaration's own lines. A file's hash, a hash of
//     a line range, or a hash of a longer span is not that body, so a caller that
//     read only part of the declaration, or a window of the file, is never treated as
//     holding the whole of it and is sent the body.
//   - The symbol must name that declaration, so a hash cannot be borrowed for a
//     different one.
//
// An acknowledgement is not a read. No body is delivered for it, so nothing is
// recorded against the agent's read tracker and no edit guard is printed: the
// caller still needs a real read of the file before a strict-mode edit.

// contextMaxHave is the most acknowledgements honoured in one call (gate v1
// work_caps.have_acks). Entries beyond it are ignored and said so; their bodies
// are delivered in full, which is the safe direction.
const contextMaxHave = 16

// haveStats is what the call did with the acknowledgements.
type haveStats struct {
	Offered   int // entries the caller sent
	Ignored   int // entries beyond the cap
	Matched   int // entries that stood in for a body
	Unmatched int // honoured entries that matched no body in this pack
}

// applyHave marks each body an entry acknowledges as held. It reads only the
// bodies already in the pack: it opens no file and records nothing.
func (p *contextPack) applyHave(have []contextHave) {
	p.Have = haveStats{Offered: len(have)}
	if len(have) > contextMaxHave {
		p.Have.Ignored = len(have) - contextMaxHave
		have = have[:contextMaxHave]
	}
	if len(have) == 0 {
		return
	}
	used := make([]bool, len(have))
	hold := func(n topology.Node, b *contextBody) {
		if b.State != bodyReady {
			return
		}
		for i, h := range have {
			if !used[i] && h.ContentSHA256 == b.SHA && h.names(p.Root, n) {
				b.Held, used[i] = true, true
				return
			}
		}
	}
	for i, s := range p.Seeds {
		if s.Kind == seedSymbol && i < len(p.Bodies) {
			hold(s.node(), &p.Bodies[i])
		}
	}
	for i := range p.Related {
		hold(p.Related[i].Node, &p.Related[i].Body)
	}
	for _, u := range used {
		if u {
			p.Have.Matched++
		}
	}
	p.Have.Unmatched = len(have) - p.Have.Matched
}

// names reports whether the entry's symbol names declaration n. The symbol takes
// the forms a seed does (path#Selector, or a bare selector) and the case gold's
// path::Selector; a selector matches n in any receiver spelling topology treats as
// equivalent, or as its bare name. A path, when there is one, must be n's file.
func (h contextHave) names(root string, n topology.Node) bool {
	where, sel := splitHaveSymbol(h.Symbol)
	if sel == "" || (sel != n.Name && !slices.Contains(topology.SelectorVariants(sel), nodeSelector(n))) {
		return false
	}
	return where == "" || haveRel(root, where) == n.Path
}

// splitHaveSymbol splits an entry's symbol at '#', or at '::' when what precedes
// it looks like a path (a selector such as Foo::bar is not cut).
func splitHaveSymbol(s string) (where, selector string) {
	s = strings.TrimSpace(s)
	if w, sel, ok := strings.Cut(s, "#"); ok {
		return strings.TrimSpace(w), strings.TrimSpace(sel)
	}
	if w, sel, ok := strings.Cut(s, "::"); ok && (strings.Contains(w, "/") || path.Ext(w) != "") {
		return strings.TrimSpace(w), strings.TrimSpace(sel)
	}
	return "", s
}

// haveRel is where as a root-relative slash path: a relative one cleaned, an
// absolute one made relative to root.
func haveRel(root, where string) string {
	if filepath.IsAbs(where) {
		return relWithinRoot(root, where)
	}
	return path.Clean(filepath.ToSlash(where))
}

// heldUnit is the status that stands in for a held body. It has no complete tier,
// so the packer never counts it as a delivered body and nothing is recorded as read.
func heldUnit(b contextBody, call string) *bodyUnit {
	full := fmt.Sprintf("    unchanged, still held: lines %d–%d (%d B) content_sha256=%s; body not re-sent. "+
		"An acknowledgement is not a read and carries no edit guard: %s", b.Start, b.End, len(b.Text), b.SHA, call)
	bare := "    unchanged, still held: content_sha256=" + b.SHA
	return &bodyUnit{file: b.File, lean: degrade(full, bare)}
}

// haveGaps says what the acknowledgements did not do.
func (p *contextPack) haveGaps() []string {
	h := p.Have
	var gaps []string
	if h.Ignored > 0 {
		gaps = append(gaps, fmt.Sprintf("have: %d entr%s beyond the %d-entry cap %s ignored; any such body in this pack is delivered in full",
			h.Ignored, textfmt.Plural(h.Ignored, "y", "ies"), contextMaxHave, textfmt.Plural(h.Ignored, "was", "were")))
	}
	if h.Unmatched > 0 {
		gaps = append(gaps, fmt.Sprintf("have: %d entr%s matched no body in this pack (its symbol is not here, or its content_sha256 is not the exact "+
			"hash of the body this pack would deliver, as a file hash or a line-range hash is not); any such body is delivered in full",
			h.Unmatched, textfmt.Plural(h.Unmatched, "y", "ies")))
	}
	return gaps
}
