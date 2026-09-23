package tools

// Independent adversarial tests for PLAN-441 (task output capping and failure
// naming), written from the spec rather than from the implementer's tests.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

var indepOmittedRe = regexp.MustCompile(`^… \((\d+) lines omitted`)

// indepGen builds total lines named "line NNNN"; lines whose index is in fail
// are replaced by a unique Go failure header.
func indepGen(total int, fail map[int]bool, trailingNL bool) string {
	parts := make([]string, total)
	for i := range parts {
		if fail[i] {
			parts[i] = fmt.Sprintf("--- FAIL: TestF%04d (0.00s)", i)
		} else {
			parts[i] = fmt.Sprintf("line %04d", i)
		}
	}
	s := strings.Join(parts, "\n")
	if trailingNL {
		s += "\n"
	}
	return s
}

func indepIsMarker(l string) bool { return strings.HasPrefix(l, "… (") }

// indepCheckLineCap asserts the spec invariants of a line-capped output whose
// input lines are all distinct (except possibly a trailing empty element).
func indepCheckLineCap(t *testing.T, in, out string) (kept []string, omitted int) {
	t.Helper()
	// A trailing newline terminates the last line; it is not a line of its own
	// (the same rule TestIndepCapTaskUnderCapUnchanged's "200 lines + NL" relies
	// on). It must be preserved, then both sides are compared without it.
	if strings.HasSuffix(in, "\n") != strings.HasSuffix(out, "\n") {
		t.Fatalf("trailing newline not preserved: in=%v out=%v", strings.HasSuffix(in, "\n"), strings.HasSuffix(out, "\n"))
	}
	in, out = strings.TrimSuffix(in, "\n"), strings.TrimSuffix(out, "\n")
	inLines := strings.Split(in, "\n")
	idx := map[string]int{}
	for i, l := range inLines {
		idx[l] = i
	}
	omitted = -1
	markers := 0
	prev := -1
	seen := map[string]bool{}
	outLines := strings.Split(out, "\n")
	for oi, l := range outLines {
		if indepIsMarker(l) {
			markers++
			if m := indepOmittedRe.FindStringSubmatch(l); m != nil {
				n, _ := strconv.Atoi(m[1])
				omitted = n
			}
			continue
		}
		i, ok := idx[l]
		if !ok {
			t.Fatalf("output line %d %q is not an input line", oi, l)
		}
		if seen[l] && l != "" {
			t.Fatalf("line %q printed twice", l)
		}
		seen[l] = true
		if i <= prev {
			t.Fatalf("line %q (input %d) out of order after input %d", l, i, prev)
		}
		prev = i
		kept = append(kept, l)
	}
	if omitted < 0 {
		t.Fatalf("no omitted-count marker in output:\n%s", out)
	}
	if len(kept) > maxTaskLines {
		t.Fatalf("kept %d content lines, budget %d", len(kept), maxTaskLines)
	}
	if markers > 2 {
		t.Fatalf("%d marker lines, want at most 2", markers)
	}
	if got := len(kept) + omitted; got != len(inLines) {
		t.Fatalf("kept %d + omitted %d = %d, want total %d", len(kept), omitted, got, len(inLines))
	}
	if outLines[len(outLines)-1] != inLines[len(inLines)-1] {
		t.Fatalf("final line %q not preserved (got %q)", inLines[len(inLines)-1], outLines[len(outLines)-1])
	}
	if !strings.HasPrefix(out, strings.Join(inLines[:taskHeadLines], "\n")+"\n") {
		t.Fatalf("head of %d lines not kept", taskHeadLines)
	}
	return kept, omitted
}

func indepCountFailures(ls []string) int {
	n := 0
	for _, l := range ls {
		if isFailureLine(l) {
			n++
		}
	}
	return n
}

func TestIndepCapTaskUnderCapUnchanged(t *testing.T) {
	cases := map[string]string{
		"empty":                   "",
		"one line":                "ok",
		"200 lines no NL":         indepGen(200, nil, false),
		"199 lines + NL":          indepGen(199, nil, true),
		"200 lines + NL":          indepGen(200, nil, true), // 200 real lines: spec says unchanged
		"200 CRLF lines no NL":    strings.ReplaceAll(indepGen(200, nil, false), "\n", "\r\n"),
		"exactly maxTaskBytes":    strings.Repeat("x", maxTaskBytes),
		"just under bytes w/ NLs": strings.Repeat(strings.Repeat("y", 1023)+"\n", 100),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := capTaskOutput(in); got != in {
				t.Fatalf("under-cap input changed (len %d -> %d); head of output: %.200q", len(in), len(got), got)
			}
		})
	}
}

func TestIndepCapTaskLineInvariants(t *testing.T) {
	type tc struct {
		total    int
		fail     []int
		trailNL  bool
		wantLift int // failures expected inside the lifted block (-1 = don't check)
	}
	cases := map[string]tc{
		"201 no NL":                       {201, nil, false, 0},
		"202 with NL":                     {201, nil, true, 0},
		"5000 no failures":                {5000, nil, false, 0},
		"failure last head line (19)":     {1000, []int{19}, false, 0},
		"failure first omitted line (20)": {1000, []int{20}, false, 1},
		"failure first tail line":         {1000, []int{1000 - 180}, false, 0},
		"failure last omitted line":       {1000, []int{1000 - 181}, false, 1},
		// 20,21,818,819 are in the middle; lifting them shrinks the tail past 820
		// and 821, which are lifted in turn: 6. 19 is head, 999 is tail.
		"boundary pack":         {1000, []int{19, 20, 21, 818, 819, 820, 821, 999}, true, 6},
		"failures only in tail": {1000, []int{900, 950, 999}, false, 0},
		// Lifting line 20 shrinks the tail by one, which moves 820 into the middle,
		// so it is lifted too: both survive, and both are in the lifted block.
		"middle + first tail line": {1000, []int{20, 820}, false, 2},
		"201 lines failure at 20":  {201, []int{20}, false, 1},
	}
	// >60 failures spread across the middle.
	var many []int
	for i := 100; i < 700; i += 5 {
		many = append(many, i)
	}
	cases["120 middle failures"] = tc{1000, many, false, 60}
	// Dense failures straddling the tail boundary.
	var dense []int
	for i := 780; i < 900; i++ {
		dense = append(dense, i)
	}
	cases["dense across tail boundary"] = tc{1000, dense, false, -1}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fail := map[int]bool{}
			for _, i := range c.fail {
				fail[i] = true
			}
			in := indepGen(c.total, fail, c.trailNL)
			out := capTaskOutput(in)
			kept, _ := indepCheckLineCap(t, in, out)
			inFail := indepCountFailures(strings.Split(in, "\n"))
			keptFail := indepCountFailures(kept)
			if inFail <= maxLiftedFailureLines && keptFail != inFail {
				t.Fatalf("kept %d of %d failure lines; all should survive", keptFail, inFail)
			}
			if inFail > maxLiftedFailureLines && keptFail < maxLiftedFailureLines {
				t.Fatalf("kept only %d failure lines of %d", keptFail, inFail)
			}
			hasLiftNote := strings.Contains(out, "failure lines from among them are kept")
			if c.wantLift >= 0 {
				if (c.wantLift > 0) != hasLiftNote {
					t.Fatalf("lift note present=%v, want lifted=%d\n%s", hasLiftNote, c.wantLift, out)
				}
				if c.wantLift > 0 && !strings.Contains(out, fmt.Sprintf("; %d failure lines from among them", c.wantLift)) {
					t.Fatalf("marker does not say %d failure lines kept:\n%.600s", c.wantLift, out)
				}
			}
		})
	}
}

func TestIndepCapTaskIdenticalFailureLines(t *testing.T) {
	// Non-unique failure text ("FAIL" repeated); count-based check only.
	parts := make([]string, 1000)
	for i := range parts {
		parts[i] = fmt.Sprintf("log %d", i)
		if i%7 == 0 && i > 30 && i < 700 {
			parts[i] = "FAIL"
		}
	}
	out := capTaskOutput(strings.Join(parts, "\n"))
	content := 0
	fails := 0
	for _, l := range strings.Split(out, "\n") {
		if indepIsMarker(l) {
			continue
		}
		content++
		if l == "FAIL" {
			fails++
		}
	}
	if content > maxTaskLines {
		t.Fatalf("content %d > %d", content, maxTaskLines)
	}
	if fails != maxLiftedFailureLines {
		t.Fatalf("lifted %d FAIL lines, want %d", fails, maxLiftedFailureLines)
	}
}

func TestIndepCapTaskCRLF(t *testing.T) {
	parts := make([]string, 600)
	for i := range parts {
		parts[i] = fmt.Sprintf("noise %d", i)
	}
	parts[100] = "--- FAIL: TestCRLF (0.00s)"
	parts[200] = "test crlf::case ... FAILED"
	parts[300] = "FAIL"
	in := strings.Join(parts, "\r\n") + "\r\n"
	out := capTaskOutput(in)
	for _, want := range []string{"--- FAIL: TestCRLF", "test crlf::case ... FAILED", "\nFAIL\r"} {
		if !strings.Contains(out, want) {
			t.Errorf("CRLF output lost failure line %q", want)
		}
	}
	if got := failedTestNames(in); strings.Join(got, ",") != "TestCRLF,crlf::case" {
		t.Errorf("failedTestNames on CRLF = %q", got)
	}
}

func TestIndepIsFailureLine(t *testing.T) {
	pos := []string{
		"--- FAIL: TestA (0.00s)",
		"    --- FAIL: TestA/sub_case (0.00s)",
		"FAIL",
		"FAIL\tgithub.com/x/y\t0.12s",
		"FAIL github.com/x/y",
		"panic: runtime error: index out of range",
		"fatal error: concurrent map writes",
		"    foo_test.go:42: got 1, want 2",
		"\tfoo_test.go:7: boom",
		"FAILED tests/test_x.py::test_y - AssertionError: nope",
		"E   assert 1 == 2",
		"E       +  where 1 = f()",
		"test tests::it_works ... FAILED",
		"test result: FAILED. 1 passed; 1 failed",
	}
	neg := []string{
		"",
		"ok  \tgithub.com/x/y\t0.1s",
		"PASS",
		"--- PASS: TestA (0.00s)",
		"2026/01/01 retrying after FAIL from upstream",
		"log: --- FAIL: not really",
		"FAILURE: build",
		"FAILED", // pytest needs a name after it
		"xFAIL",
		"  FAIL",           // indented FAIL is not the package verdict
		"Error: something", // not pytest's E-prefix
		"E assert x",       // single space: not pytest's E-line
		"    foo.go:42: not a test file",
		"foo_test.go:42: unindented",
		"test tests::it_works ... ok",
		"test result: ok. 2 passed",
		"we saw test a ... FAILED earlier",
		"the panic: word mid-line",
	}
	for _, l := range pos {
		if !isFailureLine(l) {
			t.Errorf("isFailureLine(%q) = false, want true", l)
		}
	}
	for _, l := range neg {
		if isFailureLine(l) {
			t.Errorf("isFailureLine(%q) = true, want false", l)
		}
	}
}

func TestIndepFailedTestNames(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"passing", "ok  \tpkg\t0.1s\n--- PASS: TestA (0.00s)\n", nil},
		{"go with subtests and dup", "=== RUN TestA\n--- FAIL: TestA (0.00s)\n    --- FAIL: TestA/sub (0.00s)\n--- FAIL: TestB (0.1s)\n--- FAIL: TestA (0.00s)\nFAIL\n", []string{"TestA", "TestA/sub", "TestB"}},
		{"pytest", "E   assert 1\nFAILED tests/a.py::test_one - AssertionError\nFAILED tests/a.py::test_two\n", []string{"tests/a.py::test_one", "tests/a.py::test_two"}},
		{"cargo", "test a::b ... ok\ntest a::c ... FAILED\ntest result: FAILED. 1 passed\n", []string{"a::c"}},
		{"mixed order", "test z ... FAILED\n--- FAIL: TestY (0s)\nFAILED x.py::t\ntest z ... FAILED\n", []string{"z", "TestY", "x.py::t"}},
		{"mid-line mention ignored", "log: --- FAIL: TestNope\nsaw FAILED y\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := failedTestNames(c.in)
			if strings.Join(got, "|") != strings.Join(c.want, "|") || len(got) != len(c.want) {
				t.Fatalf("failedTestNames = %q, want %q", got, c.want)
			}
		})
	}
}

// indepCheckByteCap asserts the byte-cap shape: a head prefix, a marker, a
// tail suffix, within budget, valid UTF-8 when the input was.
func indepCheckByteCap(t *testing.T, in, out string) {
	t.Helper()
	const marker = "\n… (output over 100 KiB; middle omitted)\n"
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no middle-omitted marker")
	}
	head, tail := out[:i], out[i+len(marker):]
	if !strings.HasPrefix(in, head) {
		t.Fatalf("head is not a prefix of the input")
	}
	if !strings.HasSuffix(in, tail) {
		t.Fatalf("tail is not a suffix of the input")
	}
	if len(head)+len(tail) > maxTaskBytes {
		t.Fatalf("kept %d bytes, budget %d", len(head)+len(tail), maxTaskBytes)
	}
	if utf8.ValidString(in) && !utf8.ValidString(out) {
		t.Errorf("output is not valid UTF-8 though the input was")
	}
	// The final (non-empty) line must survive: at least its last bytes.
	last := strings.TrimRight(in, "\n")
	if j := strings.LastIndexByte(last, '\n'); j >= 0 {
		last = last[j+1:]
	}
	suffix := last
	if len(suffix) > 64 {
		suffix = suffix[len(suffix)-64:]
	}
	if !strings.Contains(tail, suffix) {
		t.Errorf("final line's end %q lost; tail is %d bytes", suffix, len(tail))
	}
}

func TestIndepCapTaskBytes(t *testing.T) {
	longLines := func(n, w int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "%05d %s\n", i, strings.Repeat("z", w))
		}
		return b.String()
	}
	cases := map[string]string{
		"maxTaskBytes+1 one line":          strings.Repeat("x", maxTaskBytes+1),
		"one enormous ASCII line, no NL":   strings.Repeat("a", 300_000),
		"one enormous multibyte line":      strings.Repeat("世", 100_000),
		"multibyte 2-byte, no NL":          strings.Repeat("é", 80_000) + "END",
		"150 x 1KiB lines":                 longLines(150, 1000),
		"150 x 1KiB lines no trailing NL":  strings.TrimSuffix(longLines(150, 1000), "\n"),
		"huge final line with trailing NL": longLines(20, 100) + strings.Repeat("q", 120_000) + "TAILMARK\n",
		"huge final line no trailing NL":   longLines(20, 100) + strings.Repeat("q", 120_000) + "TAILMARK",
		"huge line then short verdict":     longLines(20, 100) + strings.Repeat("q", 120_000) + "\nFAIL\tpkg 1s\n",
		"head has no NL, tail has lines":   strings.Repeat("h", 50_000) + "\n" + longLines(60, 1000),
		"multibyte lines":                  strings.Repeat(strings.Repeat("ü", 400)+"\n", 300),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := capTaskOutput(in)
			if len(strings.Split(in, "\n")) > maxTaskLines {
				// Line cap ran first; the byte-cap shape only applies if still over.
				in = capTaskLines(in, maxTaskLines)
			}
			if len(in) <= maxTaskBytes {
				if out != in {
					t.Fatalf("under byte cap but changed")
				}
				return
			}
			indepCheckByteCap(t, in, out)
		})
	}
}

func TestIndepCapTaskLinesThenBytes(t *testing.T) {
	// 400 long lines with failures in the middle and a verdict at the end.
	parts := make([]string, 400)
	for i := range parts {
		parts[i] = fmt.Sprintf("%04d %s", i, strings.Repeat("n", 800))
	}
	parts[150] = "--- FAIL: TestMiddle (0.00s)"
	parts[399] = "FAIL\tgithub.com/x/y\t1.0s"
	out := capTaskOutput(strings.Join(parts, "\n"))
	if len(out) > maxTaskBytes+200 {
		t.Fatalf("output %d bytes, over budget", len(out))
	}
	if !strings.HasSuffix(out, parts[399]) {
		t.Fatalf("final verdict lost")
	}
	if !utf8.ValidString(out) {
		t.Fatalf("invalid UTF-8")
	}
}

func TestIndepKilledByLine(t *testing.T) {
	if got := killedByLine("ok\nPASS\n"); !strings.Contains(got, "no failing test named") {
		t.Fatalf("no-name case: %q", got)
	}
	if got := killedByLine("--- FAIL: TestOne (0s)\nFAIL\n"); !strings.Contains(got, "killed by: TestOne\n") {
		t.Fatalf("one name: %q", got)
	}
	var b strings.Builder
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "--- FAIL: T%d (0s)\n", i)
	}
	got := killedByLine(b.String())
	if !strings.Contains(got, "killed by: T1, T2, T3, T4, T5 (+3 more)") || strings.Contains(got, "T6") {
		t.Fatalf("eight names: %q", got)
	}
	var five strings.Builder
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&five, "--- FAIL: T%d (0s)\n", i)
	}
	if got := killedByLine(five.String()); strings.Contains(got, "more") || !strings.Contains(got, "T5") {
		t.Fatalf("exactly five: %q", got)
	}
}

func TestIndepExcerpt(t *testing.T) {
	// Failure lines present: only those, first 12, then an ellipsis.
	var b strings.Builder
	for i := range 50 {
		fmt.Fprintf(&b, "noise %d\n", i)
		if i%3 == 0 {
			fmt.Fprintf(&b, "--- FAIL: TestE%d (0s)\n", i)
		}
	}
	got := excerpt(b.String())
	if strings.Contains(got, "noise") {
		t.Fatalf("excerpt shows noise despite failure lines:\n%s", got)
	}
	if !strings.Contains(got, "TestE0 ") || !strings.Contains(got, "TestE33 ") || strings.Contains(got, "TestE36 ") {
		t.Fatalf("excerpt did not keep the first 12 failures:\n%s", got)
	}
	if !strings.HasSuffix(got, "      | …\n") {
		t.Fatalf("excerpt of >12 failures lacks trailing ellipsis:\n%s", got)
	}
	// No failure lines: last 12.
	b.Reset()
	for i := range 40 {
		fmt.Fprintf(&b, "log %02d\n", i)
	}
	got = excerpt(b.String())
	if strings.Contains(got, "log 27") || !strings.Contains(got, "log 28") || !strings.Contains(got, "log 39") {
		t.Fatalf("tail excerpt wrong:\n%s", got)
	}
	// Failure after trailing log noise still wins.
	got = excerpt("--- FAIL: TestLate (0s)\n" + strings.Repeat("after log\n", 30))
	if !strings.Contains(got, "TestLate") {
		t.Fatalf("failure hidden by trailing noise:\n%s", got)
	}
	if excerpt("  \n\n") != "" {
		t.Fatalf("blank output should give empty excerpt")
	}
}

func TestIndepMutationVerdictKilled(t *testing.T) {
	r := mutationResult{
		outcome: MutationKilled,
		compile: stepOutcome{ran: true},
		test: stepOutcome{ran: true, exitCode: 1, output: strings.Repeat("log line\n", 30) +
			"--- FAIL: TestKiller (0.00s)\n    k_test.go:9: boom\nFAIL\tpkg\t0.1s\n" + strings.Repeat("after\n", 20)},
	}
	got := mutationVerdictLines(r)
	for _, want := range []string{"killed by: TestKiller", "| --- FAIL: TestKiller", "|     k_test.go:9: boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("verdict lacks %q:\n%s", want, got)
		}
	}
	r.test.output = "exit status 1\n"
	if got := mutationVerdictLines(r); !strings.Contains(got, "no failing test named") || !strings.Contains(got, "| exit status 1") {
		t.Errorf("no-name verdict:\n%s", got)
	}
}
