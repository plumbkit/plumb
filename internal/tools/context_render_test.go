package tools

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// busyPack is a pack with enough seeds, misses, candidates and gaps that a small
// budget has to choose. Every line is distinct so the tests can tell which
// survived.
func busyPack(root string) contextPack {
	p := contextPack{Root: root, Intent: contextIntentChange, MaxBytes: contextMaxBytesCap}
	for i := range 6 {
		p.Seeds = append(p.Seeds, contextSeed{
			Kind: seedSymbol, Path: fmt.Sprintf("pkg%d/file%d.go", i, i), Abs: fmt.Sprintf("%s/pkg%d/file%d.go", root, i, i),
			Selector: fmt.Sprintf("(*T%d).Method", i), NodeKind: "method", Line: 10 + i, Language: "go",
		})
	}
	p.Seeds = append(p.Seeds, contextSeed{
		Kind:     seedFile,
		Path:     "web/é.svelte",
		Abs:      root + "/web/é.svelte",
		Language: "svelte",
		Coverage: "svelte is not covered by an extractor: symbols and relations are unknown, not absent",
	})
	for i := range 3 {
		m := contextMiss{Input: fmt.Sprintf("Dup%d", i), Ambiguous: true, Reason: "3 declarations match; none was chosen", More: i + 1}
		for j := range 2 {
			m.Candidates = append(m.Candidates, contextCandidate{Path: fmt.Sprintf("d%d/f%d.go", i, j), Selector: fmt.Sprintf("Dup%d", i), NodeKind: "function", Line: j + 1})
		}
		p.Misses = append(p.Misses, m)
	}
	p.Misses = append(p.Misses, contextMiss{Input: "gone.go", Reason: "file not found"})
	p.Gaps = []string{"gap one is here", "gap two is here", "gap three is here"}
	return p
}

func splitFooter(t *testing.T, out string) (body []string, omitted int, hasFooter bool) {
	t.Helper()
	lines := strings.Split(out, "\n")
	if last := lines[len(lines)-1]; strings.HasPrefix(last, "omitted: ") {
		if _, err := fmt.Sscanf(last, "omitted: %d ", &omitted); err != nil {
			t.Fatalf("unparseable footer %q: %v", last, err)
		}
		return lines[:len(lines)-1], omitted, true
	}
	return lines, 0, false
}

// Every budget from the smallest accepted upward: the pack never overruns, the
// header always leads, the omitted count is exact, and what is dropped is the
// least important material.
func TestRenderContextPack_BudgetSweep(t *testing.T) {
	p := busyPack("/work/café/project")
	all := p.lines()
	full := renderContextPack(p)
	if got := strings.Count(full, "\n") + 1; got != len(all) {
		t.Fatalf("the unbounded render has %d lines but the pack has %d", got, len(all))
	}
	var omissions, complete int
	for mb := contextMinMaxBytes; mb <= contextMinMaxBytes+len(full)+64; mb++ {
		p.MaxBytes = mb
		// The header names the budget, so the lines are rebuilt for each one.
		all = p.lines()
		prioOf := map[string]int{}
		for _, l := range all {
			prioOf[l.text] = l.prio
		}
		out := renderContextPack(p)
		if len(out) > p.budget() {
			t.Fatalf("max_bytes %d: %d bytes, over the %d-byte pack budget", mb, len(out), p.budget())
		}
		if !utf8.ValidString(out) {
			t.Fatalf("max_bytes %d: the output is not valid UTF-8", mb)
		}
		body, omitted, hasFooter := splitFooter(t, out)
		if !strings.HasPrefix(body[0], "context_for_task root=") {
			t.Fatalf("max_bytes %d: the first line is %q, not the header", mb, body[0])
		}
		if len(body)+omitted != len(all) {
			t.Fatalf("max_bytes %d: %d shown + %d omitted != %d lines", mb, len(body), omitted, len(all))
		}
		if hasFooter != (omitted > 0) {
			t.Fatalf("max_bytes %d: footer present = %v with %d omitted", mb, hasFooter, omitted)
		}
		if omitted == 0 {
			complete++
		} else {
			omissions++
		}
		checkPriorityOrder(t, mb, body, all, prioOf)
		checkNoDanglingHeading(t, mb, body)
	}
	if omissions == 0 || complete == 0 {
		t.Errorf("the sweep saw %d omitting and %d complete renders; it must see both", omissions, complete)
	}
}

// checkPriorityOrder asserts that nothing less important than a dropped body
// line was kept: dropping is strictly from the bottom of the priority order.
func checkPriorityOrder(t *testing.T, mb int, shown []string, all []packLine, prioOf map[string]int) {
	t.Helper()
	kept := map[string]bool{}
	worstKept := -1
	for _, s := range shown {
		kept[s] = true
		worstKept = max(worstKept, prioOf[s])
	}
	for _, l := range all {
		if !kept[l.text] && !l.heading && l.prio < worstKept {
			t.Fatalf("max_bytes %d: dropped %q (priority %d) while keeping a line of priority %d", mb, l.text, l.prio, worstKept)
		}
	}
}

func checkNoDanglingHeading(t *testing.T, mb int, shown []string) {
	t.Helper()
	for i, s := range shown {
		isHeading := s == "gaps:" || s == "next:" || strings.HasPrefix(s, "seeds (")
		if !isHeading {
			continue
		}
		if i+1 >= len(shown) || !strings.HasPrefix(shown[i+1], "  ") {
			t.Fatalf("max_bytes %d: heading %q has no body line under it", mb, s)
		}
	}
}

// The first line is read by baselineBytesFrom looking for read_file's
// "baseline=" stamp. The root is caller-influenced text, so it must not be able
// to forge one.
func TestRenderContextPack_HeaderCannotForgeAReadStamp(t *testing.T) {
	p := busyPack("/work/baseline=999/project")
	p.MaxBytes = 12000
	out := renderContextPack(p)
	first := strings.SplitN(out, "\n", 2)[0]
	if strings.Contains(first, "baseline=") {
		t.Errorf("the first line carries a read stamp: %q", first)
	}
	if !strings.Contains(first, "baseline:999") {
		t.Errorf("the root should still be recognisable in the header: %q", first)
	}
}

func TestRenderContextPack_HeaderIsBoundedWhateverTheRoot(t *testing.T) {
	long := "/" + strings.Repeat("deep/", 80) + "é"
	p := busyPack(long)
	p.MaxBytes = contextMinMaxBytes
	out := renderContextPack(p)
	if first := strings.SplitN(out, "\n", 2)[0]; len(first) > 260 {
		t.Errorf("header is %d bytes for a %d-byte root: %q", len(first), len(long), first)
	}
	if len(out) > p.budget() {
		t.Errorf("a long root overran the minimum budget: %d > %d", len(out), p.budget())
	}
}

func TestRenderContextPack_ClampIsDisclosedInTheHeader(t *testing.T) {
	p := busyPack("/r")
	p.MaxBytes, p.ClampedFrom = contextMaxBytesCap, 99999
	if first := strings.SplitN(renderContextPack(p), "\n", 2)[0]; !strings.Contains(first, "(clamped from 99999)") {
		t.Errorf("header does not disclose the clamp: %q", first)
	}
}

// Paths and selectors come from files on disk; a terminal control sequence in a
// name must be shown, not executed.
func TestRenderContextPack_ControlCharactersAreShownNotEmitted(t *testing.T) {
	p := contextPack{Root: "/r", Intent: contextIntentUnderstand, MaxBytes: 12000}
	p.Seeds = []contextSeed{
		{Kind: seedSymbol, Path: "a\x1b[2Jb.go", Abs: "/r/ab.go", Selector: "Evil\x07", NodeKind: "function", Line: 1, Language: "go"},
		{Kind: seedFile, Path: "c\x1b]52;c;x\x07.go", Abs: "/r/c.go", Language: "go"},
	}
	p.Misses = []contextMiss{{Input: "x\x1b[31m", Reason: "r", Candidates: []contextCandidate{{Path: "p\x1bq.go", Selector: "S\x1b", NodeKind: "function", Line: 2}}}}
	out := renderContextPack(p)
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\x07') {
		t.Errorf("a raw control character reached the output: %q", out)
	}
}

func TestCallText_KeepsSelectorsLegible(t *testing.T) {
	got := callText("read_symbol", "path", "/r/a.go", "name", "Box<T>.Get & more")
	want := `read_symbol {"path":"/r/a.go","name":"Box<T>.Get & more"}`
	if got != want {
		t.Errorf("callText = %s, want %s", got, want)
	}
}

func TestPackContextLines_KeepsTheHeaderEvenWhenNothingElseFits(t *testing.T) {
	lines := []packLine{
		{text: "HEADER", prio: prioHeader},
		{text: strings.Repeat("x", 300), prio: prioSeed},
	}
	out := packContextLines(lines, len("HEADER")+len(omittedFooter(2))+1)
	if !strings.HasPrefix(out, "HEADER\n") || strings.Contains(out, "xxx") {
		t.Errorf("got %q, want the header plus the omission footer only", out)
	}
	if !strings.HasSuffix(out, omittedFooter(1)) {
		t.Errorf("the footer must state exactly one omitted line: %q", out)
	}
}
