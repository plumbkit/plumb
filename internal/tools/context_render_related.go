package tools

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// context_render_related.go — the ranked neighbourhood and the gap candidates.
//
// Each related declaration is one line: its evidence class, where it is, how it
// relates to what it was reached from, and its signature. The best-ranked few are
// followed by the body they may carry; the packer degrades those first. A gap
// candidate is a different thing and is rendered apart: it is a possibility the
// call graph cannot confirm, so it never sits in the list of relationships.

// evidenceName is the word that goes beside an evidence ordinal.
func evidenceName(e int) string {
	switch e {
	case evidenceExtractor:
		return "extractor"
	case evidenceDerived:
		return "derived"
	case evidenceHeuristic:
		return "heuristic"
	}
	return "gap candidate"
}

// relatedText is the line for one related declaration. A withheld one shows its
// location and nothing the file says: no signature, no doc.
func relatedText(r contextRelated) string {
	n := r.Node
	var sb strings.Builder
	fmt.Fprintf(&sb, "  [e%d %s] %s — %s:%d %s; %s", r.Evidence, evidenceName(r.Evidence),
		textfmt.TerminalSafeLine(nodeSelector(n)), textfmt.TerminalSafeLine(n.Path), n.StartLine, n.Kind,
		textfmt.TerminalSafeLine(r.Via))
	if r.Source != "" && !r.Root {
		fmt.Fprintf(&sb, " (%s %.1f)", textfmt.TerminalSafeLine(r.Source), r.Conf)
	}
	if r.Stale {
		sb.WriteString(" — possibly stale: its file changed since indexing")
	}
	if r.Withheld {
		sb.WriteString(" — location only: sensitive path, content withheld")
		return sb.String()
	}
	if sig := signatureDoc(n.Signature, firstLine(n.Docstring)); sig != "" {
		sb.WriteString(" — " + sig)
	}
	return sb.String()
}

// gapText is the line for one gap candidate, worded so it cannot be read as a
// caller.
func gapText(r contextRelated) string {
	n := r.Node
	return fmt.Sprintf("  %s:%d %s (%s) — %s: unresolved receiver calls possible — not a resolved caller",
		textfmt.TerminalSafeLine(n.Path), n.StartLine, textfmt.TerminalSafeLine(nodeSelector(n)), n.Kind,
		textfmt.TerminalSafeLine(r.Via))
}

func (p *contextPack) relatedHeading() string {
	n := 0
	for _, r := range p.Related {
		if !r.Gap {
			n++
		}
	}
	return fmt.Sprintf("related (%d, ranked; e3 extractor edge, e2 derived edge, e1 heuristic edge):", n)
}

const gapCandidateHeading = "gap candidates (e0, unconfirmed; the call graph cannot say whether they call the seeds):"

// topRelated marks the window of related entries that carry priority prioRelTop
// and may carry a body: the first contextRelatedBodies ranked relationships, gap
// candidates excluded. Every consumer of the window reads this one answer (the
// lines, the bodies read, the memory refs), and a withheld node holds its place in
// it, so the three cannot count it differently.
func (p *contextPack) topRelated() []bool {
	top := make([]bool, len(p.Related))
	shown := 0
	for i, r := range p.Related {
		if r.Gap {
			continue
		}
		top[i] = shown < contextRelatedBodies
		shown++
	}
	return top
}

// relatedLines renders the ranked list. The first contextRelatedBodies carry
// priority prioRelTop with the body they may have; the rest are one-line pointers
// that go first when the budget is short.
func (p *contextPack) relatedLines() []packLine {
	var ls []packLine
	top := p.topRelated()
	for i, r := range p.Related {
		if r.Gap {
			continue
		}
		prio := prioRelRest
		if top[i] {
			prio = prioRelTop
		}
		ls = append(ls, packLine{text: relatedText(r), prio: prio, section: secRelated, class: classRelated})
		if u, ok := p.relatedBodyLine(i); ok {
			ls = append(ls, u)
		}
	}
	if len(ls) == 0 {
		return nil
	}
	head := packLine{text: p.relatedHeading(), prio: prioRelTop, section: secRelated, heading: true, class: classRelated}
	return append([]packLine{head}, ls...)
}

// relatedBodyLine is the body unit that follows related declaration i, when a body
// was read for it. A declaration whose body could not be read shows its line only.
func (p *contextPack) relatedBodyLine(i int) (packLine, bool) {
	r := p.Related[i]
	if r.Body.State != bodyReady {
		return packLine{}, false
	}
	call := callText("read_symbol", "path", absUnder(p.Root, r.Node.Path), "name", nodeSelector(r.Node))
	unit := p.readyUnit(r.Body, p.relatedHeading(), len(relatedText(r))+2, call, "")
	return packLine{prio: prioRelTop, section: secRelated, class: classBody, body: unit}, true
}

// gapCandidateLines renders the gap candidates under their own heading, with the
// same priority as the other disclosures: they are what stops a short caller list
// being read as a complete one.
func (p *contextPack) gapCandidateLines() []packLine {
	var ls []packLine
	for _, r := range p.Related {
		if r.Gap {
			ls = append(ls, packLine{text: gapText(r), prio: prioDetail, section: secGapCandidates, class: classCandidate})
		}
	}
	if len(ls) == 0 {
		return nil
	}
	head := packLine{text: gapCandidateHeading, prio: prioDetail, section: secGapCandidates, heading: true, class: classCandidate}
	return append([]packLine{head}, ls...)
}

// unreadBodyGap counts related declarations whose body was attempted and could not
// be read, with the first reason, so a missing body is accounted for.
func (p *contextPack) unreadBodyGap() []string {
	n, why := 0, ""
	for _, r := range p.Related {
		if r.Body.State == bodyUnavailable {
			if n == 0 {
				why = r.Body.Why
			}
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d related declaration(s) show a signature only, no body (%s)", n, why)}
}
