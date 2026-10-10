package tools

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// context_render.go — the compact text a context_for_task call returns.
//
// The output is a header line, then seeds, gaps and next-call sections. Every
// line carries a priority, so a tight budget drops the least important lines
// first and reports exactly how many it dropped; a line is never cut in half.

// Line priorities: lower survives longer. Candidates and gaps share a tier
// because both are disclosures a reader needs in order to trust the seed list.
const (
	prioHeader = iota
	prioSeed
	prioDetail
	prioNext
)

// Sections group a heading with its body lines, so a heading is never left
// standing over nothing.
const (
	secHeader = iota
	secSeeds
	secGaps
	secNext
)

// headerRootBytes bounds the root shown in the header line, which is never
// dropped, so a long path cannot crowd out the budget it reports.
const headerRootBytes = 96

type packLine struct {
	text    string
	prio    int
	section int
	heading bool
}

// renderContextPack renders pack within its budget.
func renderContextPack(p contextPack) string {
	return packContextLines(p.lines(), p.budget())
}

func (p *contextPack) lines() []packLine {
	ls := []packLine{{text: p.headerLine(), prio: prioHeader, section: secHeader}}
	ls = append(ls, p.seedLines()...)
	ls = append(ls, p.gapLines()...)
	return append(ls, p.nextLines()...)
}

// headerLine names the tool, the agent's root, the intent and the budget. It is
// the first line of the output, and baselineBytesFrom reads that line looking
// for read_file's "baseline=" stamp, so nothing in it may spell that key: the
// root is caller-influenced text.
func (p *contextPack) headerLine() string {
	budget := fmt.Sprintf("max_bytes=%d", p.MaxBytes)
	if p.ClampedFrom > 0 {
		budget += fmt.Sprintf(" (clamped from %d)", p.ClampedFrom)
	}
	return fmt.Sprintf("context_for_task root=%s intent=%s %s (pack<=%d, %d reserved)",
		headerSafe(p.Root), p.Intent, budget, p.budget(), contextReserveBytes)
}

// headerSafe makes untrusted text fit the header line: control characters
// shown, the stats stamp key defused, and the length bounded.
func headerSafe(s string) string {
	s = strings.ReplaceAll(textfmt.TerminalSafeLine(s), "baseline=", "baseline:")
	return textfmt.ClampBytes(s, headerRootBytes)
}

func (p *contextPack) seedLines() []packLine {
	ls := make([]packLine, 0, 1+len(p.Seeds)+len(p.Misses))
	ls = append(ls, packLine{
		text: fmt.Sprintf("seeds (%d resolved, %d unresolved):", len(p.Seeds), len(p.Misses)),
		prio: prioSeed, section: secSeeds, heading: true,
	})
	for _, s := range p.Seeds {
		ls = append(ls, packLine{text: "  " + seedText(s), prio: prioSeed, section: secSeeds})
	}
	for _, m := range p.Misses {
		ls = append(ls, missLines(m)...)
	}
	return ls
}

func seedText(s contextSeed) string {
	var sb strings.Builder
	if s.Kind == seedSymbol {
		fmt.Fprintf(&sb, "symbol %s — %s:%d %s", textfmt.TerminalSafeLine(s.Selector),
			textfmt.TerminalSafeLine(s.Path), s.Line, s.NodeKind)
	} else {
		fmt.Fprintf(&sb, "file %s", textfmt.TerminalSafeLine(s.Path))
	}
	if s.Language != "" {
		fmt.Fprintf(&sb, " [%s]", s.Language)
	}
	if s.Coverage != "" {
		fmt.Fprintf(&sb, " — coverage gap: %s", s.Coverage)
	}
	return sb.String()
}

// missLines renders a seed that did not resolve, followed by its labelled
// candidates, if any. An ambiguous selector is told how to retry; a candidate
// is shown in the path#selector form that can be pasted back into symbols.
func missLines(m contextMiss) []packLine {
	head := fmt.Sprintf("  unresolved %q: %s", textfmt.TerminalSafeLine(m.Input), m.Reason)
	if m.Ambiguous {
		head = fmt.Sprintf("  ambiguous %q: %s; retry with path#Selector using one of", textfmt.TerminalSafeLine(m.Input), m.Reason)
	}
	ls := []packLine{{text: head, prio: prioSeed, section: secSeeds}}
	for _, c := range m.Candidates {
		ls = append(ls, packLine{
			text: fmt.Sprintf("    %s#%s (%s, line %d)", textfmt.TerminalSafeLine(c.Path), textfmt.TerminalSafeLine(c.Selector), c.NodeKind, c.Line),
			prio: prioDetail, section: secSeeds,
		})
	}
	if m.More > 0 {
		ls = append(ls, packLine{text: fmt.Sprintf("    … and %d more", m.More), prio: prioDetail, section: secSeeds})
	}
	return ls
}

func (p *contextPack) gapLines() []packLine {
	if len(p.Gaps) == 0 {
		return nil
	}
	ls := []packLine{{text: "gaps:", prio: prioDetail, section: secGaps, heading: true}}
	for _, g := range p.Gaps {
		ls = append(ls, packLine{text: "  - " + strings.Join(strings.Fields(g), " "), prio: prioDetail, section: secGaps})
	}
	return ls
}

// nextLines offers one concrete follow-up call per resolved seed, with absolute
// paths so the call is valid on a connection that refuses relative ones.
func (p *contextPack) nextLines() []packLine {
	if len(p.Seeds) == 0 {
		return nil
	}
	ls := []packLine{{text: "next:", prio: prioNext, section: secNext, heading: true}}
	for _, s := range p.Seeds {
		ls = append(ls, packLine{text: "  " + nextCall(s), prio: prioNext, section: secNext})
	}
	return ls
}

func nextCall(s contextSeed) string {
	switch {
	case s.Kind == seedSymbol:
		return callText("read_symbol", "path", s.Abs, "name", s.Selector)
	case s.Coverage != "":
		return callText("read_file", "file_path", s.Abs)
	default:
		return callText("file_outline", "uri", s.Abs)
	}
}

// callText renders tool{"k":"v",...} with the pairs in the order given. Values
// are JSON-quoted without HTML escaping, so a selector such as Foo<T> stays
// legible and copyable.
func callText(tool string, kv ...string) string {
	var sb strings.Builder
	sb.WriteString(tool)
	sb.WriteString(" {")
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(jsonQuote(kv[i]))
		sb.WriteByte(':')
		sb.WriteString(jsonQuote(kv[i+1]))
	}
	sb.WriteByte('}')
	return sb.String()
}

func jsonQuote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes: invalid UTF-8 is replaced, not refused
	return strings.TrimRight(b.String(), "\n")
}

// omittedFooter is the line that discloses dropped lines. It is built from the
// same constants wherever it is measured, so the reserve and the real footer
// cannot disagree.
func omittedFooter(n int) string {
	return fmt.Sprintf("omitted: %d line(s) did not fit in max_bytes; raise it (cap %d) or narrow the seeds", n, contextMaxBytesCap)
}

// packContextLines joins lines within budget bytes. When everything fits it
// returns everything. Otherwise it keeps the longest prefix of the
// priority-ordered lines that leaves room for the omission footer, shows the
// survivors in their original order, and states exactly how many lines went.
// The header is always kept.
func packContextLines(lines []packLine, budget int) string {
	keep, omitted := selectLines(lines, budget)
	var kept []string
	for i, l := range lines {
		if keep[i] {
			kept = append(kept, l.text)
		}
	}
	if omitted > 0 {
		kept = append(kept, omittedFooter(omitted))
	}
	return strings.Join(kept, "\n")
}

// selectLines decides which lines survive budget, and how many were dropped.
func selectLines(lines []packLine, budget int) (keep []bool, omitted int) {
	keep = make([]bool, len(lines))
	all := 0
	for i, l := range lines {
		all += len(l.text)
		if i > 0 {
			all++ // the newline before it
		}
	}
	if all <= budget {
		for i := range keep {
			keep[i] = true
		}
		return keep, 0
	}
	reserve := len(omittedFooter(len(lines))) + 1 // worst-case digits, plus its newline
	keep[0] = true
	used := len(lines[0].text)
	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(lines[a].prio, lines[b].prio) })
	for _, i := range order {
		if i == 0 {
			continue
		}
		cost := len(lines[i].text) + 1
		if used+cost+reserve > budget {
			break // strict priority: nothing below a line that did not fit is shown
		}
		keep[i] = true
		used += cost
	}
	dropDanglingHeadings(lines, keep)
	for _, k := range keep {
		if !k {
			omitted++
		}
	}
	return keep, omitted
}

// dropDanglingHeadings unkeeps a heading none of whose body lines survived.
func dropDanglingHeadings(lines []packLine, keep []bool) {
	hasBody := map[int]bool{}
	for i, l := range lines {
		if keep[i] && !l.heading {
			hasBody[l.section] = true
		}
	}
	for i, l := range lines {
		if keep[i] && l.heading && !hasBody[l.section] {
			keep[i] = false
		}
	}
}
