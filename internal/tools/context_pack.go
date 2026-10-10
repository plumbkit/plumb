package tools

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// context_pack.go — the packer: fit a pack's records into a byte budget.
//
// A record is a line, or a body unit that can be shown at several fidelities. The
// pack degrades per record in a fixed order: a complete body, then a status line
// carrying the signature and doc, then a bare status line, then a counted
// omission. A body is never split, a line is never cut, and the cut is only ever
// made between records, so the output is valid UTF-8 and every omission is
// counted exactly, by class.

// Line priorities: lower survives longer. Candidates and gaps share a tier
// because both are disclosures a reader needs in order to trust the seed list.
// Bodies come after those disclosures: a pack that cannot afford a body still tells
// the truth about what it left out. The best-ranked related declarations, each with
// the body it may carry, follow the seeds' bodies. The affected test packages come
// next (they say what to run), then the constraints retrieved from memories and
// documents (evidence a reader should have, not code), then follow-up calls, and
// the one-line pointers for the rest of the ranked list last.
const (
	prioHeader = iota
	prioSeed
	prioDetail
	prioBody
	prioRelTop
	prioAffected
	prioConstraint
	prioNext
	prioRelRest
)

// packClass is what a record is, for the omission count.
type packClass int

const (
	classSeed packClass = iota
	classCandidate
	classGap
	classNext
	classBody
	classRelated
	classAffected
	classConstraint
	classCount
)

var packClassNames = [classCount]string{"seed", "candidate", "gap", "next", "body", "related", "affected", "constraint"}

// omissions counts dropped records by class.
type omissions [classCount]int

func (o omissions) total() int {
	n := 0
	for _, c := range o {
		n += c
	}
	return n
}

// packLine is one record. A plain line has only text, and may list alts: leaner
// renderings of the same record, tried in order when text does not fit. A body
// unit (body != nil) is shown at the richest fidelity that fits, and is the only
// kind of record whose delivery counts as a read.
type packLine struct {
	text    string
	alts    []string
	prio    int
	section int
	heading bool
	class   packClass
	body    *bodyUnit
}

// bodyUnit is the renderings of one symbol seed's body. full is the complete body
// block, with the file's guard line, and noGuard the same block for a file whose
// guard an earlier delivered body has already printed; both are empty when no
// complete block can ever fit. lean are status lines, richest first.
type bodyUnit struct {
	file    int // index into pack.Files
	full    string
	noGuard string
	lean    []string
}

// packResult is a rendered pack and the files whose bodies it delivered, in the
// order their first body appears. Only these files may be recorded as read.
type packResult struct {
	Text      string
	Delivered []int
}

// footerCeiling is the longest the omission footer can be, newline included. It
// is reserved whenever anything is dropped, and used by the renderer to judge
// whether a body could ever fit.
func footerCeiling() int {
	var worst omissions
	for i := range worst {
		worst[i] = 999
	}
	return len(omittedFooter(worst)) + 1
}

// omittedFooter is the record that discloses dropped records, with the exact
// count per class. It is built from the same constants wherever it is measured,
// so the reserve and the real footer cannot disagree.
func omittedFooter(o omissions) string {
	var parts []string
	for c, n := range o {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, packClassNames[c]))
		}
	}
	return fmt.Sprintf("omitted: %d item(s) (%s) did not fit in max_bytes; raise it (cap %d) or narrow the seeds",
		o.total(), strings.Join(parts, ", "), contextMaxBytesCap)
}

// packContextLines joins lines within budget bytes and returns the text.
func packContextLines(lines []packLine, budget int) string {
	return packLines(lines, budget).Text
}

// tiers lists the renderings of l, richest first, and whether the first is a
// complete body. guardDone says which files' guard lines are already printed.
func (l packLine) tiers(guardDone map[int]bool) (texts []string, complete bool) {
	if l.body == nil {
		return append([]string{l.text}, l.alts...), false
	}
	if l.body.full != "" {
		first := l.body.full
		if guardDone[l.body.file] {
			first = l.body.noGuard
		}
		return append([]string{first}, l.body.lean...), true
	}
	return l.body.lean, false
}

// packLines decides which records survive budget, at what fidelity, and renders
// them in their original order with an exact omission footer. The header is
// always kept. When everything fits at full fidelity it is all returned.
// Otherwise records are taken in strict priority order, each at the richest
// fidelity that still leaves room for the footer; the first record that fits at
// no fidelity ends the pack, so nothing less important than a dropped record is
// shown.
func packLines(lines []packLine, budget int) packResult {
	chosen, delivered := fitEverything(lines, budget)
	if chosen == nil {
		chosen, delivered = fitByPriority(lines, budget)
	}
	var dropped omissions
	var out []string
	for i, text := range chosen {
		if text == "" {
			dropped[lines[i].class]++
			continue
		}
		out = append(out, text)
	}
	if dropped.total() > 0 {
		out = append(out, omittedFooter(dropped))
	}
	return packResult{Text: strings.Join(out, "\n"), Delivered: delivered}
}

// fitEverything returns every record at its richest fidelity when that fits in
// budget, and nil otherwise.
func fitEverything(lines []packLine, budget int) (chosen []string, delivered []int) {
	chosen = make([]string, len(lines))
	guardDone := map[int]bool{}
	total := 0
	for i, l := range lines {
		texts, complete := l.tiers(guardDone)
		chosen[i] = texts[0]
		if complete {
			delivered = noteDelivered(delivered, guardDone, l.body.file)
		}
		total += len(texts[0])
		if i > 0 {
			total++ // the newline before it
		}
	}
	if total > budget {
		return nil, nil
	}
	return chosen, delivered
}

func noteDelivered(delivered []int, guardDone map[int]bool, file int) []int {
	if guardDone[file] {
		return delivered
	}
	guardDone[file] = true
	return append(delivered, file)
}

// fitByPriority is the degraded path. An empty string in the result is a dropped
// record; the header is index 0 and is always kept.
func fitByPriority(lines []packLine, budget int) (chosen []string, delivered []int) {
	chosen = make([]string, len(lines))
	chosen[0] = lines[0].text
	used := len(lines[0].text)
	avail := budget - footerCeiling()
	guardDone := map[int]bool{}
	for _, i := range priorityOrder(lines) {
		texts, complete := lines[i].tiers(guardDone)
		tier := firstThatFits(texts, avail-used)
		if tier < 0 {
			break // strict priority: nothing below a record that did not fit is shown
		}
		chosen[i] = texts[tier]
		used += len(texts[tier]) + 1
		if complete && tier == 0 {
			delivered = noteDelivered(delivered, guardDone, lines[i].body.file)
		}
	}
	dropDanglingHeadings(lines, chosen)
	return chosen, delivered
}

// priorityOrder lists the record indices, header excluded, most important first;
// records of equal priority keep their order.
func priorityOrder(lines []packLine) []int {
	order := make([]int, 0, len(lines)-1)
	for i := 1; i < len(lines); i++ {
		order = append(order, i)
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(lines[a].prio, lines[b].prio) })
	return order
}

// firstThatFits is the index of the first text that fits in room (its newline
// included), or -1.
func firstThatFits(texts []string, room int) int {
	for i, t := range texts {
		if len(t)+1 <= room {
			return i
		}
	}
	return -1
}

// dropDanglingHeadings drops a heading none of whose records survived, so the
// output never shows a heading over nothing.
func dropDanglingHeadings(lines []packLine, chosen []string) {
	hasBody := map[int]bool{}
	for i, l := range lines {
		if chosen[i] != "" && !l.heading {
			hasBody[l.section] = true
		}
	}
	for i, l := range lines {
		if chosen[i] != "" && l.heading && !hasBody[l.section] {
			chosen[i] = ""
		}
	}
}
