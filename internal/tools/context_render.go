package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// context_render.go — the compact text a context_for_task call returns.
//
// The output is a header line, then seeds (each symbol seed followed by its body
// or the reason it has none), gaps and next-call sections. Every record carries a
// priority, so a tight budget drops the least important first and reports
// exactly how many it dropped; the packer (context_pack.go) never cuts a record
// in half.

// Sections group a heading with its body lines, so a heading is never left
// standing over nothing.
const (
	secHeader = iota
	secSeeds
	secRelated
	secGapCandidates
	secAffected
	secConstraints
	secGaps
	secNext
)

// headerRootBytes bounds the root shown in the header line, which is never
// dropped, so a long path cannot crowd out the budget it reports.
const headerRootBytes = 96

// Bounds for the signature and doc shown on a status line.
const (
	statusSignatureBytes = 120
	statusDocBytes       = 80
)

// renderContextPack renders pack within its budget.
func renderContextPack(p contextPack) string {
	return renderContext(p).Text
}

// renderContext renders pack within its budget and reports which files' bodies
// it delivered, which is what the caller may record as read.
func renderContext(p contextPack) packResult {
	return packLines(p.lines(), p.budget())
}

func (p *contextPack) lines() []packLine {
	ls := []packLine{{text: p.headerLine(), prio: prioHeader, section: secHeader}}
	ls = append(ls, p.seedLines()...)
	ls = append(ls, p.relatedLines()...)
	ls = append(ls, p.gapCandidateLines()...)
	ls = append(ls, p.affectedLines()...)
	ls = append(ls, p.constraintLines()...)
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

func (p *contextPack) seedHeading() string {
	h := fmt.Sprintf("seeds (%d resolved, %d unresolved)", len(p.Seeds), len(p.Misses))
	if p.Discovery != nil {
		h += " — proposed from the task prose, none were given; pass files or symbols to choose your own"
	}
	return h + ":"
}

func (p *contextPack) seedLines() []packLine {
	ls := make([]packLine, 0, 1+2*len(p.Seeds)+len(p.Misses))
	ls = append(ls, packLine{text: p.seedHeading(), prio: prioSeed, section: secSeeds, heading: true, class: classSeed})
	for i, s := range p.Seeds {
		ls = append(ls, packLine{text: "  " + seedText(s), prio: prioSeed, section: secSeeds, class: classSeed})
		if u, ok := p.bodyLine(i); ok {
			ls = append(ls, u)
			if uses, ok := p.usesLine(p.Bodies[i], u.body, secSeeds); ok {
				ls = append(ls, uses)
			}
		}
	}
	for _, m := range p.Misses {
		ls = append(ls, missLines(m)...)
	}
	return append(ls, p.discoveryLines()...)
}

// discoveryLines says why prose discovery found nothing, when it found nothing,
// names the path-shaped tokens that are no file here, and lists the matches it
// did not take, as candidates to pass back as symbols.
func (p *contextPack) discoveryLines() []packLine {
	d := p.Discovery
	if d == nil {
		return nil
	}
	var ls []packLine
	line := func(text string, prio int, class packClass) {
		ls = append(ls, packLine{text: "  " + textfmt.TerminalSafeLine(text), prio: prio, section: secSeeds, class: class})
	}
	if note := p.discoveryNote(); note != "" {
		line(note, prioSeed, classSeed)
	}
	if len(d.NotFiles) > 0 {
		line("named in the task but no file here: "+strings.Join(d.NotFiles, ", "), prioDetail, classSeed)
	}
	if len(d.Others) == 0 {
		return ls
	}
	line("other matches (candidates, not seeds):", prioDetail, classCandidate)
	for _, c := range d.Others {
		ls = append(ls, packLine{
			text: fmt.Sprintf("    %s#%s (%s, line %d)", textfmt.TerminalSafeLine(c.Path), textfmt.TerminalSafeLine(c.Selector), c.NodeKind, c.Line),
			prio: prioDetail, section: secSeeds, class: classCandidate,
		})
	}
	return ls
}

// discoveryNote names the real reason a prose-only pack has no seed, or says
// what a search left out; "" when there is nothing to say.
func (p *contextPack) discoveryNote() string {
	d := p.Discovery
	var aside []string
	if d.Excluded > 0 {
		aside = append(aside, fmt.Sprintf("%d match(es) outside within/corpora left out", d.Excluded))
	}
	if d.Withheld > 0 {
		aside = append(aside, fmt.Sprintf("%d match(es) in sensitive files, which discovery never proposes", d.Withheld))
	}
	suffix := ""
	if len(aside) > 0 {
		suffix = " (" + strings.Join(aside, "; ") + ")"
	}
	switch {
	case d.Note != "":
		return d.Note + suffix
	case len(p.Seeds) > 0:
		if suffix == "" {
			return ""
		}
		return "search" + suffix
	case len(p.Misses) > 0:
		return "no proposal resolved to one declaration (see the unresolved lines above)" + suffix +
			"; name a file or symbol, or use workspace_search"
	case len(d.Terms) == 0:
		return "the task prose has no searchable words; name a file or symbol, or use workspace_search"
	default:
		return fmt.Sprintf("no declaration name carries the task's words (%s)%s; name a file or symbol, or use workspace_search",
			strings.Join(d.Terms, " "), suffix)
	}
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
	if s.Shadowed > 0 {
		fmt.Fprintf(&sb, " — also matches %s", referenceNodes(s.Shadowed))
	}
	if s.Coverage != "" {
		fmt.Fprintf(&sb, " — coverage gap: %s", s.Coverage)
	}
	switch s.Origin {
	case originNamed:
		sb.WriteString(" — " + originNamed)
	case originFound:
		fmt.Fprintf(&sb, " — %s on: %s", originFound, textfmt.TerminalSafeLine(s.Matched))
	}
	return sb.String()
}

// bodyLine is the body unit that follows symbol seed i: its complete body when
// the budget allows, else a status line saying why it is not here and how to get
// it. A file seed, or a seed whose body was never attempted, has none.
func (p *contextPack) bodyLine(i int) (packLine, bool) {
	if i >= len(p.Bodies) || p.Bodies[i].State == bodyNone {
		return packLine{}, false
	}
	s, b := p.Seeds[i], p.Bodies[i]
	call := callText("read_symbol", "path", s.Abs, "name", readSymbolName(s.Selector, s.Name))
	sig := signatureDoc(b.Signature, b.Doc)
	unit := &bodyUnit{file: b.File}
	if b.State != bodyReady {
		bare := fmt.Sprintf("    body unavailable (%s): %s", b.Why, call)
		unit.lean = degrade(withSignature(bare, sig), bare)
	} else {
		unit = p.readyUnit(b, p.seedHeading(), len(seedText(s))+2, call, sig)
	}
	return packLine{prio: prioBody, section: secSeeds, class: classBody, body: unit}, true
}

// readyUnit builds the renderings of a body that exists. It is offered whole only
// when it could fit alone in the budget under heading and its own line of lineLen
// bytes; a body larger than anything the budget can hold becomes a handoff that
// names the size and the snapshot it was taken from, so a later read can be
// checked against it. A body the caller acknowledged holding is not offered at
// all: its unit has no complete tier, so it is never a delivery.
func (p *contextPack) readyUnit(b contextBody, heading string, lineLen int, call, sig string) *bodyUnit {
	if b.Held {
		return heldUnit(b, call)
	}
	meta := fmt.Sprintf("%d B, lines %d–%d", len(b.Text), b.Start, b.End)
	head := fmt.Sprintf("    body lines %d–%d (%d B) content_sha256=%s", b.Start, b.End, len(b.Text), b.SHA)
	gutter := strings.TrimSuffix(withLineGutter(b.Text, b.Start), "\n")
	full := head + "\n" + p.guardLine(b.File) + "\n" + gutter
	if p.fitsAlone(heading, len(full), lineLen) {
		omitted := fmt.Sprintf("    body omitted for budget (%s): %s", meta, call)
		bare := "    body omitted for budget: " + call
		return &bodyUnit{
			file: b.File, full: full, noGuard: head + "\n" + gutter,
			lean: degrade(withSignature(omitted, sig), omitted, bare),
		}
	}
	big := fmt.Sprintf("    body larger than the budget (%s; content_sha256=%s; snapshot file sha256=%s): %s",
		meta, b.SHA, p.Files[b.File].SHA, call)
	bare := fmt.Sprintf("    body larger than the budget (%s): %s", meta, call)
	return &bodyUnit{file: b.File, lean: degrade(withSignature(big, sig), big, bare)}
}

// guardLine is the edit guard for a file's snapshot, in the shape read_symbol's
// header uses so its values can be copied into edit_file's expected_mtime and
// expected_sha. It belongs to the file; a body's content_sha256 is not one.
func (p *contextPack) guardLine(file int) string {
	f := p.Files[file]
	return fmt.Sprintf("    guard for edit_file on %s (expected_mtime, expected_sha): mtime=%s sha256=%s",
		textfmt.TerminalSafeLine(f.Path), f.MTime.Format(time.RFC3339Nano), f.SHA)
}

// fitsAlone reports whether a block of blockLen bytes could ever be delivered:
// whether it fits in the budget next to the header, its section heading, its own
// line and the omission footer, with every other record dropped.
func (p *contextPack) fitsAlone(heading string, blockLen, lineLen int) bool {
	fixed := len(p.headerLine()) + 1 + len(heading) + 1 + lineLen + 1 + footerCeiling()
	return fixed+blockLen+1 <= p.budget()
}

// signatureDoc is the one-line description a status line carries: the signature,
// and the first line of the doc comment if there is one. Both are index text.
func signatureDoc(sig, doc string) string {
	var parts []string
	if sig != "" {
		parts = append(parts, textfmt.ClampBytes(textfmt.TerminalSafeLine(sig), statusSignatureBytes))
	}
	if doc != "" {
		parts = append(parts, "// "+textfmt.ClampBytes(textfmt.TerminalSafeLine(doc), statusDocBytes))
	}
	return strings.Join(parts, " ")
}

func withSignature(base, sig string) string {
	if sig == "" {
		return base
	}
	return base + " — " + sig
}

// degrade drops empty and repeated renderings, keeping the richest first.
func degrade(tiers ...string) []string {
	var out []string
	for _, t := range tiers {
		if t != "" && (len(out) == 0 || out[len(out)-1] != t) {
			out = append(out, t)
		}
	}
	return out
}

// missLines renders a seed that did not resolve, followed by its labelled
// candidates, if any. An ambiguous selector is told how to retry; a candidate
// is shown in the path#selector form that can be pasted back into symbols.
func missLines(m contextMiss) []packLine {
	reason := textfmt.TerminalSafeLine(m.Reason)
	head := fmt.Sprintf("  unresolved %q: %s", textfmt.TerminalSafeLine(m.Input), reason)
	if m.Ambiguous {
		head = fmt.Sprintf("  ambiguous %q: %s; retry with path#Selector using one of", textfmt.TerminalSafeLine(m.Input), reason)
	}
	ls := []packLine{{text: head, prio: prioSeed, section: secSeeds, class: classSeed}}
	for _, c := range m.Candidates {
		ls = append(ls, packLine{
			text: fmt.Sprintf("    %s#%s (%s, line %d)", textfmt.TerminalSafeLine(c.Path), textfmt.TerminalSafeLine(c.Selector), c.NodeKind, c.Line),
			prio: prioDetail, section: secSeeds, class: classCandidate,
		})
	}
	if m.More > 0 {
		ls = append(ls, packLine{text: fmt.Sprintf("    … and %d more", m.More), prio: prioDetail, section: secSeeds, class: classCandidate})
	}
	return ls
}

func (p *contextPack) gapLines() []packLine {
	if len(p.Gaps) == 0 {
		return nil
	}
	ls := []packLine{{text: "gaps:", prio: prioDetail, section: secGaps, heading: true, class: classGap}}
	for _, g := range p.Gaps {
		ls = append(ls, packLine{text: "  - " + textfmt.TerminalSafeLine(strings.Join(strings.Fields(g), " ")), prio: prioDetail, section: secGaps, class: classGap})
	}
	return ls
}

// nextLines offers one concrete follow-up call per resolved seed that has no body
// record of its own (a symbol seed's status line already carries its call), with
// absolute paths so the call is valid on a connection that refuses relative ones.
func (p *contextPack) nextLines() []packLine {
	var calls []string
	for i, s := range p.Seeds {
		if i < len(p.Bodies) && p.Bodies[i].State != bodyNone {
			continue
		}
		calls = append(calls, nextCall(s))
	}
	if len(calls) == 0 {
		return nil
	}
	ls := []packLine{{text: "next:", prio: prioNext, section: secNext, heading: true, class: classNext}}
	for _, c := range calls {
		ls = append(ls, packLine{text: "  " + c, prio: prioNext, section: secNext, class: classNext})
	}
	return ls
}

func nextCall(s contextSeed) string {
	switch {
	case s.Kind == seedSymbol:
		return callText("read_symbol", "path", s.Abs, "name", readSymbolName(s.Selector, s.Name))
	case s.Coverage != "":
		return callText("read_file", "file_path", s.Abs)
	default:
		return callText("file_outline", "uri", s.Abs)
	}
}

// readSymbolName is the name a follow-up read_symbol call carries for a declaration
// the index spells selector (its qualified name, or the bare name when it has none)
// and calls name. The pack must hand over a spelling read_symbol accepts whether or
// not a language server answers: with one, "Recv.Method" and "(*Recv).Method" both
// resolve, but the topology fallback (no server, a cold one) matches a dotted name
// on whole identifier segments and so refuses the Go receiver spelling. "Recv.Method"
// resolves on both routes, so a Go method is rendered that way (a generic receiver
// loses its type parameters, as read_symbol's own name paths do). Any other selector
// that is not one identifier, a dot and the declaration's name has no spelling both
// routes are known to share, so the bare name stands: it resolves everywhere, and
// lists every declaration of that name in the file instead of choosing one.
func readSymbolName(selector, name string) string {
	if recv, method, ok := goMethodReceiver(selector); ok {
		if recv = stripTypeParams(recv); isIdentifier(recv) {
			return recv + "." + method
		}
		return method
	}
	if parent, child, ok := strings.Cut(selector, "."); ok && child == name && isIdentifier(parent) {
		return selector
	}
	if name != "" {
		return name
	}
	return selector
}

// isIdentifier reports whether s is a non-empty run of letters, digits and
// underscores, the characters read_symbol's topology fallback treats as one name
// segment.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
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
