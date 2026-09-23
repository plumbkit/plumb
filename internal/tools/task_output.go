package tools

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// task_output.go — bounding a task command's output without losing the verdict.
//
// A test runner's verdict lands at the END of its output, and the lines that
// say WHICH test failed can sit anywhere in between. Keeping the first N lines —
// what capTaskOutput used to do — kept a noisy package's log output and dropped
// both, so a red `go test` through run_task named no test at all (PLAN-441).
// The cap now keeps a short head, the tail, and up to maxLiftedFailureLines
// failure-marker lines from the omitted middle.

const (
	// taskHeadLines is the head kept when output is over maxTaskLines: enough to
	// show what started, not enough to crowd out the verdict.
	taskHeadLines = 20
	// maxLiftedFailureLines caps the failure lines rescued from the omitted
	// middle, so a run where hundreds of tests fail still fits the line budget.
	maxLiftedFailureLines = 60
)

// Failure markers come in two ranks. Anchored at line start (after optional
// indentation) so a log line merely MENTIONING "FAIL" matches neither.
//
// A VERDICT line says a test or package failed. A DETAIL line is the assertion
// text around one — but the same shape is also printed for a PASSING test's
// t.Log under `go test -v`, so details alone can fill any budget with noise.
// Selection therefore always takes verdicts first (selectFailureLines).
var (
	verdictLinePatterns = []*regexp.Regexp{
		// Go: "--- FAIL: TestX (0.1s)", "FAIL\tpkg 1.2s", "FAIL", panics.
		regexp.MustCompile(`^\s*--- FAIL: `),
		regexp.MustCompile(`^FAIL(\s|$)`),
		regexp.MustCompile(`^(panic: |fatal error: )`),
		// pytest: "FAILED tests/x.py::test_y - AssertionError".
		regexp.MustCompile(`^FAILED `),
		// cargo test: "test foo ... FAILED", "test result: FAILED.".
		regexp.MustCompile(`^test \S+ \.\.\. FAILED$`),
		regexp.MustCompile(`^test result: FAILED`),
	}
	detailLinePatterns = []*regexp.Regexp{
		// Go: the indented "file_test.go:42: message" under a --- FAIL.
		regexp.MustCompile(`^\s+\S+_test\.go:\d+: `),
		// pytest: "E   assert 1 == 2".
		regexp.MustCompile(`^E\s{2,}\S`),
	}
)

// Failure-line ranks, highest kept first.
const (
	notFailure = iota
	failureDetail
	failureVerdict
)

// failureRank classifies line. A trailing CR is ignored, so CRLF output matches
// the `$`-anchored patterns.
func failureRank(line string) int {
	line = strings.TrimSuffix(line, "\r")
	for _, re := range verdictLinePatterns {
		if re.MatchString(line) {
			return failureVerdict
		}
	}
	for _, re := range detailLinePatterns {
		if re.MatchString(line) {
			return failureDetail
		}
	}
	return notFailure
}

// isFailureLine reports whether line is a test runner's failure marker.
func isFailureLine(line string) bool { return failureRank(line) != notFailure }

// selectFailureLines picks at most limit failure lines from lines — every
// verdict first, then details with what budget remains — and returns them in
// their original order.
func selectFailureLines(lines []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	pick := make([]bool, len(lines))
	taken := 0
	for _, rank := range []int{failureVerdict, failureDetail} {
		for i, l := range lines {
			if taken == limit {
				break
			}
			if !pick[i] && failureRank(l) == rank {
				pick[i] = true
				taken++
			}
		}
	}
	out := make([]string, 0, taken)
	for i, l := range lines {
		if pick[i] {
			out = append(out, l)
		}
	}
	return out
}

// goFailedTest matches Go's per-test failure header; pytest and cargo name the
// test on their own marker lines, handled in failedTestNames.
var (
	goFailedTest    = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	pytestFailed    = regexp.MustCompile(`^FAILED (\S+)`)
	cargoTestFailed = regexp.MustCompile(`^test (\S+) \.\.\. FAILED$`)
)

// failedTestNames returns the distinct names of the tests the output reports
// as failed, in order of first appearance.
func failedTestNames(out string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		for _, re := range []*regexp.Regexp{goFailedTest, pytestFailed, cargoTestFailed} {
			if m := re.FindStringSubmatch(line); m != nil && !seen[m[1]] {
				seen[m[1]] = true
				names = append(names, m[1])
			}
		}
	}
	return names
}

// capTaskOutput bounds output to maxTaskLines lines, then maxTaskBytes bytes,
// keeping the head, the tail, and the failure lines in between.
func capTaskOutput(s string) string {
	return capTaskBytes(capTaskLines(s, maxTaskLines))
}

// capTaskLines keeps taskHeadLines lines, then up to maxLiftedFailureLines
// failure lines from the omitted middle (verdicts first), then as much tail as
// the budget allows, with a marker naming how many lines were dropped. A
// trailing newline ends the last line rather than starting another, so it does
// not count against the cap.
func capTaskLines(s string, maxLines int) string {
	body, nl := strings.CutSuffix(s, "\n")
	lines := strings.Split(body, "\n")
	if len(lines) <= maxLines {
		return s
	}
	head := min(taskHeadLines, maxLines/4)
	base := len(lines) - (maxLines - head) // tail start with no lines lifted
	// Every lifted line costs one tail line, and shrinking the tail can expose
	// another failure line to the middle. liftedCount is non-decreasing in n and
	// capped, so iterating to its fixed point converges; stopping short dropped a
	// failure line sitting at the head of the shrunken-away tail. The lift always
	// leaves at least one tail line when the budget has one, so the final line —
	// where a runner's verdict lands — survives even a cap full of failures.
	// Each pass counts only the lines the previous one newly exposed, so the
	// whole walk is linear however many passes it takes.
	liftCap := min(max(maxLines-head-1, 0), maxLiftedFailureLines)
	found := countFailureLines(lines, head, base)
	n := 0
	for {
		next := min(found, liftCap)
		if next == n {
			break
		}
		found += countFailureLines(lines, base+n, base+next)
		n = next
	}
	tailStart := base + n
	lifted := selectFailureLines(lines[head:tailStart], n)
	omitted := tailStart - head - len(lifted)

	var b strings.Builder
	if head > 0 {
		b.WriteString(strings.Join(lines[:head], "\n"))
		b.WriteByte('\n')
	}
	if len(lifted) > 0 {
		fmt.Fprintf(&b, "… (%d lines omitted; %d failure lines from among them are kept below)\n", omitted, len(lifted))
		b.WriteString(strings.Join(lifted, "\n"))
		b.WriteString("\n… (end of kept failure lines)\n")
	} else {
		fmt.Fprintf(&b, "… (%d lines omitted)\n", omitted)
	}
	if tailStart == len(lines) {
		// No tail at all (a zero budget): end on the marker, not a blank line.
		return strings.TrimSuffix(b.String(), "\n")
	}
	b.WriteString(strings.Join(lines[tailStart:], "\n"))
	if nl {
		b.WriteByte('\n')
	}
	return b.String()
}

// countFailureLines counts the failure lines in lines[from:to].
func countFailureLines(lines []string, from, to int) int {
	n := 0
	for i := from; i < to; i++ {
		if isFailureLine(lines[i]) {
			n++
		}
	}
	return n
}

// explainsFailure reports whether out carries a verdict that says more than a
// bare `FAIL` — a named test, a panic (a `go test` timeout names no test but
// prints `panic: test timed out`), a fatal error.
func explainsFailure(out string) bool {
	if len(failedTestNames(out)) > 0 {
		return true
	}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if failureRank(l) == failureVerdict && !bareFailVerdict.MatchString(l) {
			return true
		}
	}
	return false
}

// bareFailVerdict is Go's package-level FAIL line, which says a package failed
// but not why — including when a test file did not compile.
var bareFailVerdict = regexp.MustCompile(`^FAIL(\s|$)`)

// capTaskBytes bounds s to maxTaskBytes, keeping a fifth from the head and the
// rest from the tail. It cuts at a line boundary when one is available and at a
// rune boundary otherwise, so the result is always valid UTF-8 for valid input
// and the final line always survives (in part, if it alone is over budget).
func capTaskBytes(s string) string {
	if len(s) <= maxTaskBytes {
		return s
	}
	headBudget := maxTaskBytes / 5
	tailBudget := maxTaskBytes - headBudget
	head := s[:headBudget]
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		head = head[:i]
	} else {
		// Drop a rune the cut split in two. Bounded by UTFMax, so bytes that were
		// already invalid in the input are left alone rather than eaten.
		for range utf8.UTFMax {
			if r, size := utf8.DecodeLastRuneInString(head); r != utf8.RuneError || size != 1 {
				break
			}
			head = head[:len(head)-1]
		}
	}
	tail := s[len(s)-tailBudget:]
	// A newline that is the tail's own last byte would cut the whole final line.
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
		tail = tail[i+1:]
	} else {
		for i := 0; i < utf8.UTFMax && len(tail) > 0 && !utf8.RuneStart(tail[0]); i++ {
			tail = tail[1:]
		}
	}
	return head + "\n… (output over 100 KiB; middle omitted)\n" + tail
}
