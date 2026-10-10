package tools

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// context_pack_test.go — the packer's budget, degradation and accounting.

var footerRe = regexp.MustCompile(`^omitted: (\d+) item\(s\) \(([^)]*)\) did not fit in max_bytes; raise it \(cap \d+\) or narrow the seeds$`)

// parseFooter reads the omission footer off the last line of out. ok is false
// when there is none.
func parseFooter(t *testing.T, out string) (counts omissions, total int, ok bool) {
	t.Helper()
	last := out[strings.LastIndex(out, "\n")+1:]
	if !strings.HasPrefix(last, "omitted: ") {
		return counts, 0, false
	}
	m := footerRe.FindStringSubmatch(last)
	if m == nil {
		t.Fatalf("unparseable footer %q", last)
	}
	total, _ = strconv.Atoi(m[1])
	for _, part := range strings.Split(m[2], ", ") {
		var n int
		var name string
		if _, err := fmt.Sscanf(part, "%d %s", &n, &name); err != nil {
			t.Fatalf("unparseable footer class %q in %q", part, last)
		}
		found := false
		for c, cn := range packClassNames {
			if cn == name {
				counts[c], found = n, true
			}
		}
		if !found {
			t.Fatalf("unknown class %q in footer %q", name, last)
		}
	}
	return counts, total, true
}

// presentTiers lists every text a record could have been shown as.
func presentTiers(l packLine) []string {
	if l.body == nil {
		return append([]string{l.text}, l.alts...)
	}
	var out []string
	for _, s := range append([]string{l.body.full, l.body.noGuard}, l.body.lean...) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func isShown(out string, l packLine) bool {
	for _, tier := range presentTiers(l) {
		if strings.Contains(out, tier) {
			return true
		}
	}
	return false
}

// synthLines builds a pack's worth of records with every class and a few multi-byte
// characters, each text distinct so a test can tell which survived.
func synthLines() []packLine {
	block := func(tag string, n int) (full, noGuard string) {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "%3d\tbody %s line %d é\n", i+1, tag, i)
		}
		head := "    body lines 1–" + strconv.Itoa(n) + " content_sha256=" + tag
		return head + "\n    guard " + tag + "\n" + strings.TrimSuffix(b.String(), "\n"), head + "\n" + strings.TrimSuffix(b.String(), "\n")
	}
	fullA, noGuardA := block("A", 12)
	fullB, noGuardB := block("B", 12)
	fullC, noGuardC := block("C", 6)
	unit := func(file int, full, noGuard, tag string) packLine {
		return packLine{prio: prioBody, section: secSeeds, class: classBody, body: &bodyUnit{
			file: file, full: full, noGuard: noGuard,
			lean: []string{"    body omitted for budget (rich " + tag + "): read_symbol {\"name\":\"" + tag + "\"} — func " + tag + "() é", "    body omitted for budget: " + tag},
		}}
	}
	return []packLine{
		{text: "HEADER é", prio: prioHeader},
		{text: "seeds (3 resolved, 1 unresolved):", prio: prioSeed, section: secSeeds, heading: true, class: classSeed},
		{text: "  symbol A — a.go:1", prio: prioSeed, section: secSeeds, class: classSeed},
		unit(0, fullA, noGuardA, "A"),
		{text: "  symbol B — a.go:20", prio: prioSeed, section: secSeeds, class: classSeed},
		unit(0, fullB, noGuardB, "B"),
		{text: "  symbol C — b.go:3", prio: prioSeed, section: secSeeds, class: classSeed},
		unit(1, fullC, noGuardC, "C"),
		{text: "  unresolved \"Dup\": 2 declarations match", prio: prioSeed, section: secSeeds, class: classSeed},
		{text: "    d0/f0.go#Dup (function, line 1)", prio: prioDetail, section: secSeeds, class: classCandidate},
		{text: "    d1/f1.go#Dup (function, line 2)", prio: prioDetail, section: secSeeds, class: classCandidate},
		{text: "gaps:", prio: prioDetail, section: secGaps, heading: true, class: classGap},
		{text: "  - gap one is here", prio: prioDetail, section: secGaps, class: classGap},
		{text: "  - gap two is here", prio: prioDetail, section: secGaps, class: classGap},
		{text: "next:", prio: prioNext, section: secNext, heading: true, class: classNext},
		{text: "  file_outline {\"uri\":\"/r/a.go\"}", prio: prioNext, section: secNext, class: classNext},
		{text: "  file_outline {\"uri\":\"/r/b.go\"}", prio: prioNext, section: secNext, class: classNext},
	}
}

// Every budget from nothing to roomy: the output never exceeds the budget it was
// given, is valid UTF-8, keeps the header first, drops strictly from the bottom of
// the priority order, never shows part of a body, and the footer's per-class
// counts are exactly the records that are not there.
func TestPackLines_BudgetSweepIsExactAndNeverSplitsABody(t *testing.T) {
	lines := synthLines()
	fullOut := packLines(lines, 1<<20).Text
	var sawDropped, sawLean, sawFull, sawPartialBudget bool
	for budget := len(lines[0].text) + footerCeiling(); budget <= len(fullOut)+8; budget++ {
		res := packLines(lines, budget)
		out := res.Text
		if len(out) > budget {
			t.Fatalf("budget %d: output is %d bytes", budget, len(out))
		}
		if !utf8.ValidString(out) || !strings.HasPrefix(out, "HEADER é") {
			t.Fatalf("budget %d: invalid UTF-8 or no header first:\n%s", budget, out)
		}
		var want omissions
		droppedSoFar := false
		for _, i := range priorityOrder(lines) {
			l := lines[i]
			shown := isShown(out, l)
			if !shown {
				want[l.class]++
			}
			if !l.heading {
				if droppedSoFar && shown {
					t.Fatalf("budget %d: %q survived after a more important record was dropped", budget, presentTiers(l)[0])
				}
				droppedSoFar = droppedSoFar || !shown
			}
		}
		got, total, hasFooter := parseFooter(t, out)
		if got != want || total != want.total() || hasFooter != (want.total() > 0) {
			t.Fatalf("budget %d: footer counts %v (total %d, present %v), want %v\n%s", budget, got, total, hasFooter, want, out)
		}
		checkBodiesWhole(t, budget, out, lines, res.Delivered)
		sawDropped = sawDropped || want.total() > 0
		sawLean = sawLean || strings.Contains(out, "body omitted for budget")
		sawFull = sawFull || len(res.Delivered) > 0
		sawPartialBudget = sawPartialBudget || (len(res.Delivered) > 0 && strings.Contains(out, "body omitted for budget"))
	}
	for name, saw := range map[string]bool{
		"a dropped record": sawDropped, "a status-only body": sawLean, "a delivered body": sawFull,
		"a mix of delivered and degraded bodies": sawPartialBudget,
	} {
		if !saw {
			t.Errorf("the sweep never produced %s, so it proved nothing about it", name)
		}
	}
}

// checkBodiesWhole asserts that every body line in out belongs to a block that is
// shown whole, and that Delivered names exactly the files whose block is.
func checkBodiesWhole(t *testing.T, budget int, out string, lines []packLine, delivered []int) {
	t.Helper()
	fileDelivered, wantLines := map[int]bool{}, 0
	for _, l := range lines {
		if l.body == nil || l.body.full == "" {
			continue
		}
		if strings.Contains(out, l.body.full) || strings.Contains(out, l.body.noGuard) {
			fileDelivered[l.body.file] = true
			wantLines += strings.Count(l.body.noGuard, "\n") // a head line, then one line per body line
		}
	}
	// Every shown body line must belong to a whole block, so a part-shown block
	// cannot hide.
	bodyLines := 0
	for _, ln := range strings.Split(out, "\n") {
		if gutterRe.MatchString(ln) {
			bodyLines++
		}
	}
	if bodyLines != wantLines {
		t.Fatalf("budget %d: %d body lines shown but whole blocks account for %d; a body was split:\n%s", budget, bodyLines, wantLines, out)
	}
	if len(delivered) != len(fileDelivered) {
		t.Fatalf("budget %d: Delivered = %v but whole blocks of files %v are shown", budget, delivered, fileDelivered)
	}
	for _, f := range delivered {
		if !fileDelivered[f] {
			t.Fatalf("budget %d: Delivered names file %d, whose body is not shown whole", budget, f)
		}
	}
}

// The guard travels with the first body of a file that is actually delivered: when
// the first is dropped and the second survives, the second carries it.
func TestPackLines_GuardMovesToTheFirstDeliveredBody(t *testing.T) {
	lines := synthLines()
	// Make A's block too large to ever fit while B's fits.
	lines[3].body.full = lines[3].body.full + strings.Repeat("\n  1\tpadding padding padding padding", 40)
	lines[3].body.noGuard = lines[3].body.noGuard + strings.Repeat("\n  1\tpadding padding padding padding", 40)
	res := packLines(lines, len(lines[0].text)+footerCeiling()+1300)
	if strings.Contains(res.Text, "guard A") || strings.Contains(res.Text, lines[3].body.noGuard) {
		t.Fatalf("setup: body A should not have been delivered:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, lines[5].body.full) {
		t.Errorf("body B is the first of its file to be delivered, so it must carry the guard:\n%s", res.Text)
	}
	if res.Delivered[0] != 0 {
		t.Errorf("Delivered = %v, want file 0 first", res.Delivered)
	}
}

func TestPackLines_HeaderAlwaysSurvivesAndFooterIsReserved(t *testing.T) {
	lines := synthLines()
	budget := len(lines[0].text) + footerCeiling()
	res := packLines(lines, budget)
	body, _, hasFooter := splitFooter(t, res.Text)
	if !hasFooter || len(body) != 1 || body[0] != lines[0].text || len(res.Text) > budget {
		t.Errorf("at the smallest budget want the header and an exact footer, got %q", res.Text)
	}
	if len(res.Delivered) != 0 {
		t.Errorf("no body fits at the smallest budget, yet files %v were delivered", res.Delivered)
	}
}

// The whole served response stays within max_bytes across the legal range: the pack
// within max_bytes minus the reserve, valid UTF-8, header first, and the footer
// counts exactly the records that are not there. The sweep must see every outcome,
// or its agreement would prove nothing.
func TestContextForTask_BudgetNeverExceededAndOmissionsAreExactAcrossMaxBytes(t *testing.T) {
	s := newShop(t)
	args := func(mb int) map[string]any {
		return map[string]any{
			"files": []string{"cart/cart.go", "web/CartBadge.svelte"},
			"symbols": []string{
				"cart/cart.go#Cart.Add", "cart/cart.go#Cart.Total", "reports/reports.go#Summarise",
				"reports/reports.go#StatusLabel", "Total", "cart/cart.go#Cart.Coupon",
			},
			"max_bytes": mb,
		}
	}
	budgets := []int{contextMinMaxBytes, contextMinMaxBytes + 1, contextMaxBytesCap}
	for mb := contextMinMaxBytes; mb < contextMaxBytesCap; mb += 41 {
		budgets = append(budgets, mb)
	}
	saw := map[string]bool{}
	for _, mb := range budgets {
		pack := s.collect(t, args(mb))
		out, err := s.run(t, args(mb))
		if err != nil {
			t.Fatalf("max_bytes %d: %v", mb, err)
		}
		if len(out) > mb-contextReserveBytes {
			t.Fatalf("max_bytes %d: %d bytes, over the %d B pack budget", mb, len(out), mb-contextReserveBytes)
		}
		if !utf8.ValidString(out) || !strings.HasPrefix(out, "context_for_task root=") {
			t.Fatalf("max_bytes %d: invalid UTF-8 or no header first", mb)
		}
		var want omissions
		for _, l := range pack.lines()[1:] {
			if !isShown(out, l) {
				want[l.class]++
			}
		}
		got, total, hasFooter := parseFooter(t, out)
		if got != want || total != want.total() || hasFooter != (want.total() > 0) {
			t.Fatalf("max_bytes %d: footer counts %v (total %d), want %v\n%s", mb, got, total, want, out)
		}
		// A body is only worth its bytes once the disclosures that outrank it (the
		// seed lines, candidates and gaps) are all there.
		if strings.Contains(out, "    body lines ") {
			for _, l := range pack.lines()[1:] {
				if l.prio < prioBody && !l.heading && !isShown(out, l) {
					t.Fatalf("max_bytes %d: a body was delivered while %q, which outranks it, was dropped\n%s", mb, presentTiers(l)[0], out)
				}
			}
		}
		noteOutcomes(saw, out, hasFooter, mb)
	}
	for _, outcome := range []string{"delivered", "status", "handoff", "cap", "omission", "clean"} {
		if !saw[outcome] {
			t.Errorf("the sweep never produced outcome %q, so it proved nothing about it", outcome)
		}
	}
}

func noteOutcomes(saw map[string]bool, out string, hasFooter bool, mb int) {
	saw["delivered"] = saw["delivered"] || strings.Contains(out, "    body lines ")
	saw["status"] = saw["status"] || strings.Contains(out, "body omitted for budget")
	saw["handoff"] = saw["handoff"] || strings.Contains(out, "body larger than the budget")
	saw["cap"] = saw["cap"] || strings.Contains(out, "source-read cap reached")
	saw["omission"] = saw["omission"] || hasFooter
	saw["clean"] = saw["clean"] || (!hasFooter && mb > 20000)
}
