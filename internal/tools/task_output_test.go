package tools

import (
	"fmt"
	"strings"
	"testing"
)

// redGoTestOutput mimics the run that motivated PLAN-441: a package that logs
// hundreds of lines, one failing test in the middle, and more logging after it.
func redGoTestOutput(before, after int) string {
	var b strings.Builder
	for i := range before {
		fmt.Fprintf(&b, "2026/09/23 09:28:47 INFO daemon: session re-pinned n=%d\n", i)
	}
	b.WriteString("--- FAIL: TestAfterToolFilesAGitCallUnderItsRepo (0.14s)\n")
	b.WriteString("    conn_attribution_test.go:344: /tmp/x has 0 git rows, want 1\n")
	for i := range after {
		fmt.Fprintf(&b, "2026/09/23 09:28:48 INFO daemon: logical agent registered n=%d\n", i)
	}
	b.WriteString("FAIL\n")
	b.WriteString("FAIL\tgithub.com/plumbkit/plumb/internal/cli\t2.144s")
	return b.String()
}

func TestCapTaskOutputKeepsTheFailureFromTheMiddle(t *testing.T) {
	got := capTaskOutput(redGoTestOutput(400, 400))

	for _, want := range []string{
		"--- FAIL: TestAfterToolFilesAGitCallUnderItsRepo",
		"conn_attribution_test.go:344: /tmp/x has 0 git rows, want 1",
		"FAIL\tgithub.com/plumbkit/plumb/internal/cli",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("capped output lost %q", want)
		}
	}
	if n := strings.Count(got, "--- FAIL: "); n != 1 {
		t.Errorf("the failure header appears %d times, want exactly 1", n)
	}
	// The budget holds: maxTaskLines of content plus the two marker lines.
	if n := strings.Count(got, "\n") + 1; n > maxTaskLines+2 {
		t.Errorf("capped output is %d lines, over the %d-line budget", n, maxTaskLines+2)
	}
	if !strings.Contains(got, "lines omitted; 2 failure lines from among them are kept below") {
		t.Errorf("the omission marker must say failure lines were kept:\n%s", got)
	}
}

func TestCapTaskOutputKeepsTheTailAndSaysWhatItDropped(t *testing.T) {
	var b strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	got := capTaskOutput(strings.TrimSuffix(b.String(), "\n"))
	if !strings.Contains(got, "line999") {
		t.Error("the last line — where a runner's verdict lands — was dropped")
	}
	if !strings.HasPrefix(got, "line0\n") {
		t.Error("the head was dropped")
	}
	if !strings.Contains(got, "… (800 lines omitted)") {
		t.Errorf("the marker must count the omitted lines:\n%s", got)
	}
}

func TestCapTaskOutputLeavesShortOutputAlone(t *testing.T) {
	in := redGoTestOutput(3, 3)
	if got := capTaskOutput(in); got != in {
		t.Errorf("output under the cap must pass through unchanged, got:\n%s", got)
	}
}

// A run with hundreds of failures must still fit: the lifted lines are capped.
func TestCapTaskOutputCapsTheLiftedFailures(t *testing.T) {
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "--- FAIL: TestN%d (0.00s)\n", i)
	}
	got := capTaskOutput(b.String())
	if n := strings.Count(got, "\n") + 1; n > maxTaskLines+4 {
		t.Errorf("capped output is %d lines, over budget", n)
	}
}

// PR #502 review: under `go test -v` a PASSING test's t.Log lines have the same
// shape as assertion detail. Verdicts are selected first, so they cannot evict
// the one real --- FAIL from the 60-line lift.
func TestCapTaskOutputPrefersVerdictsOverDetail(t *testing.T) {
	var b strings.Builder
	for i := range 300 {
		fmt.Fprintf(&b, "    x_test.go:%d: t.Log from a passing test\n", i+1)
	}
	b.WriteString("--- FAIL: TestReal (0.01s)\n")
	for i := range 300 {
		fmt.Fprintf(&b, "    y_test.go:%d: more passing-test logging\n", i+1)
	}
	got := capTaskOutput(b.String())
	if !strings.Contains(got, "--- FAIL: TestReal") {
		t.Error("t.Log detail lines evicted the only verdict from the lift")
	}
	if ex := excerpt(got); !strings.Contains(ex, "--- FAIL: TestReal") {
		t.Errorf("the excerpt showed detail noise instead of the verdict:\n%s", ex)
	}
}

// PR #502 review: the lift is bounded by the tail budget, so a small cap with
// many failures neither panics nor overruns.
func TestCapTaskLinesSmallBudgets(t *testing.T) {
	in := strings.Repeat("--- FAIL: TestN (0.00s)\n", 300) + "LAST\n"
	for _, maxLines := range []int{0, 1, 3, 10, 50, 79} {
		got := capTaskLines(in, maxLines)
		if maxLines > 0 && !strings.HasSuffix(got, "\nLAST\n") {
			t.Errorf("maxLines=%d lost the final line to a cap full of failures", maxLines)
		}
		content := 0
		for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			if !strings.HasPrefix(l, "… (") {
				content++
			}
		}
		if content > maxLines {
			t.Errorf("maxLines=%d kept %d content lines", maxLines, content)
		}
	}
}

// PR #502 re-review: a `go test` timeout names no test but panics; the panic
// line is the explanation and must reach the excerpt, not goroutine frames.
func TestExcerptShowsTheTimeoutPanic(t *testing.T) {
	var b strings.Builder
	b.WriteString("panic: test timed out after 30s\n\trunning tests:\n\t\tTestSlow (30s)\n")
	for i := range 40 {
		fmt.Fprintf(&b, "goroutine %d [chan receive]:\n\tmain.f()\n", i)
	}
	b.WriteString("FAIL\texample.com/p\t30.1s\nFAIL")
	if ex := excerpt(b.String()); !strings.Contains(ex, "panic: test timed out") {
		t.Errorf("the timeout panic did not reach the excerpt:\n%s", ex)
	}
}

func TestSelectFailureLinesNonPositiveLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		if got := selectFailureLines([]string{"FAIL", "FAIL"}, limit); len(got) != 0 {
			t.Errorf("limit %d selected %d lines, want 0", limit, len(got))
		}
	}
}

// PR #502 review: a test FILE that fails to compile prints only bare FAIL
// verdicts, which name no test. The excerpt must fall back to the tail, where
// the compile error that explains the kill is.
func TestExcerptShowsTheCompileErrorWhenNoTestIsNamed(t *testing.T) {
	out := "./p_test.go:5:2: undefined: Foo\nFAIL\texample.com/p [build failed]\nFAIL"
	if ex := excerpt(out); !strings.Contains(ex, "undefined: Foo") {
		t.Errorf("the compile error was hidden behind bare FAIL lines:\n%s", ex)
	}
}

func TestCapTaskBytesKeepsTheTail(t *testing.T) {
	long := strings.Repeat(strings.Repeat("x", 1000)+"\n", 150) + "VERDICT"
	got := capTaskOutput(long)
	if len(got) > maxTaskBytes+64 {
		t.Errorf("capped output is %d bytes, over the %d-byte cap", len(got), maxTaskBytes)
	}
	if !strings.HasSuffix(got, "VERDICT") {
		t.Error("the byte cap dropped the tail")
	}
	if !strings.Contains(got, "middle omitted") {
		t.Error("the byte cap must say it cut the middle")
	}
}

func TestIsFailureLineIgnoresMentionsOfFail(t *testing.T) {
	for _, line := range []string{
		"2026/09/23 INFO the --- FAIL: header is parsed here",
		"INFO: FAILED to connect, retrying",
		"test result: ok. 3 passed; 0 failed",
	} {
		if isFailureLine(line) {
			t.Errorf("a log line mentioning failure was treated as a marker: %q", line)
		}
	}
	for _, line := range []string{
		"--- FAIL: TestX (0.1s)",
		"    --- FAIL: TestX/sub (0.0s)",
		"FAIL",
		"FAIL\tpkg\t0.1s",
		"panic: runtime error",
		"    x_test.go:12: got 1 want 2",
		"FAILED tests/test_a.py::test_b - assert 1 == 2",
		"E       assert 1 == 2",
		"test tests::it_works ... FAILED",
		"test result: FAILED. 1 passed; 1 failed",
	} {
		if !isFailureLine(line) {
			t.Errorf("a failure marker was not recognised: %q", line)
		}
	}
}

func TestFailedTestNames(t *testing.T) {
	out := strings.Join([]string{
		"--- FAIL: TestA (0.1s)",
		"    --- FAIL: TestA/sub (0.0s)",
		"--- FAIL: TestA (0.1s)", // repeated: reported once
		"FAILED tests/test_x.py::test_y - boom",
		"test m::t ... FAILED",
	}, "\n")
	got := strings.Join(failedTestNames(out), ",")
	if want := "TestA,TestA/sub,tests/test_x.py::test_y,m::t"; got != want {
		t.Errorf("failedTestNames = %q, want %q", got, want)
	}
}

func TestKilledByNamesTheFailingTest(t *testing.T) {
	out := capTaskOutput(redGoTestOutput(400, 400))
	if got := killedByLine(out); !strings.Contains(got, "killed by: TestAfterToolFilesAGitCallUnderItsRepo") {
		t.Errorf("killedByLine = %q", got)
	}
	if got := killedByLine("exit status 1"); !strings.Contains(got, "no failing test named") {
		t.Errorf("with no test named, the line must say so rather than invent one: %q", got)
	}
	ex := excerpt(out)
	if !strings.Contains(ex, "conn_attribution_test.go:344") {
		t.Errorf("the excerpt must show the assertion, not the trailing log noise:\n%s", ex)
	}
	if strings.Contains(ex, "logical agent registered") {
		t.Errorf("the excerpt showed log noise although failure lines existed:\n%s", ex)
	}
}
