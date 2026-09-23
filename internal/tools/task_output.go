package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// task_output.go — bounding a task command's output without losing the verdict.
//
// A test runner's verdict lands at the END of its output, and the lines that
// say WHICH test failed can sit anywhere in between. Keeping the first N lines —
// what capTaskOutput used to do — kept a noisy package's log output and dropped
// both, so a red `go test` through run_task named no test at all (PLAN-441).
// The cap now keeps a short head, the tail, and every failure-marker line from
// the omitted middle.

const (
	// taskHeadLines is the head kept when output is over maxTaskLines: enough to
	// show what started, not enough to crowd out the verdict.
	taskHeadLines = 20
	// maxLiftedFailureLines caps the failure lines rescued from the omitted
	// middle, so a run where hundreds of tests fail still fits the line budget.
	maxLiftedFailureLines = 60
)

// failureLinePatterns recognises the lines test runners use to name a failure.
// Anchored at line start (after optional indentation) so a log line merely
// MENTIONING "FAIL" is not lifted.
var failureLinePatterns = []*regexp.Regexp{
	// Go: "--- FAIL: TestX (0.1s)", "FAIL\tpkg 1.2s", "FAIL", panics, and the
	// indented "file_test.go:42: message" assertion lines under a --- FAIL.
	regexp.MustCompile(`^\s*--- FAIL: `),
	regexp.MustCompile(`^FAIL(\s|$)`),
	regexp.MustCompile(`^(panic: |fatal error: )`),
	regexp.MustCompile(`^\s+\S+_test\.go:\d+: `),
	// pytest: "FAILED tests/x.py::test_y - AssertionError", "E   assert ...".
	regexp.MustCompile(`^FAILED `),
	regexp.MustCompile(`^E\s{2,}\S`),
	// cargo test: "test foo ... FAILED", "test result: FAILED.".
	regexp.MustCompile(`^test \S+ \.\.\. FAILED$`),
	regexp.MustCompile(`^test result: FAILED`),
}

// isFailureLine reports whether line is a test runner's failure marker.
func isFailureLine(line string) bool {
	for _, re := range failureLinePatterns {
		if re.MatchString(line) {
			return true
		}
	}
	return false
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
// failure lines from the omitted middle, then as much tail as the budget allows,
// with a marker naming how many lines were dropped.
func capTaskLines(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	head := min(taskHeadLines, maxLines/4)
	// Count the failure lines outside the full-size tail first, then shrink the
	// tail by that many so the lifted lines fit the budget. Shrinking only widens
	// the middle, so re-collecting there finds at least as many again.
	tailStart := len(lines) - (maxLines - head)
	n := len(liftedBefore(lines, head, tailStart, maxLiftedFailureLines))
	tailStart += n
	lifted := liftedBefore(lines, head, tailStart, n)
	omitted := tailStart - head - len(lifted)

	var b strings.Builder
	b.WriteString(strings.Join(lines[:head], "\n"))
	if len(lifted) > 0 {
		fmt.Fprintf(&b, "\n… (%d lines omitted; the %d failure lines among them are kept below)\n", omitted, len(lifted))
		b.WriteString(strings.Join(lifted, "\n"))
		b.WriteString("\n… (end of kept failure lines)\n")
	} else {
		fmt.Fprintf(&b, "\n… (%d lines omitted)\n", omitted)
	}
	b.WriteString(strings.Join(lines[tailStart:], "\n"))
	return b.String()
}

// liftedBefore re-collects at most limit failure lines from lines[from:to].
func liftedBefore(lines []string, from, to, limit int) []string {
	var out []string
	for i := from; i < to && len(out) < limit; i++ {
		if isFailureLine(lines[i]) {
			out = append(out, lines[i])
		}
	}
	return out
}

// capTaskBytes bounds s to maxTaskBytes, keeping a fifth from the head and the
// rest from the tail, cut at line boundaries.
func capTaskBytes(s string) string {
	if len(s) <= maxTaskBytes {
		return s
	}
	headBudget := maxTaskBytes / 5
	tailBudget := maxTaskBytes - headBudget
	head := s[:headBudget]
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		head = head[:i]
	}
	tail := s[len(s)-tailBudget:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return head + "\n… (output over 100 KiB; middle omitted)\n" + tail
}
