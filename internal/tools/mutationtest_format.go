package tools

import (
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// mutationtest_format.go renders mutation_test's report. Presentation only — no
// classification happens here.
//
// The ordering is a deliberate reading order, not a data dump. An agent skims
// the last lines of a tool result, so the summary leads with the two findings
// that change what the agent should do next: a SURVIVED mutant (the assertion
// is vacuous) and an INVALID one (nothing was proven, and it must not be
// mistaken for a kill). A run where everything died says so in one line.

// formatMutationReport renders the whole run.
func formatMutationReport(args mutationTestArgs, plan mutationPlan, warnings []string, results []mutationResult) string {
	var b strings.Builder
	b.WriteString(mutationHeader(args, plan, len(results)))
	for _, w := range warnings {
		fmt.Fprintf(&b, "⚠ no git safety net: %s\n", w)
	}
	b.WriteString("\n")
	for i, r := range results {
		b.WriteString(formatMutationResult(i+1, r))
	}
	b.WriteString(mutationSummary(results))
	return b.String()
}

func mutationHeader(args mutationTestArgs, plan mutationPlan, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mutation_test — %d %s\n", n, textfmt.Plural(n, "mutant", "mutants"))
	fmt.Fprintf(&b, "  compile gate: %s (%s, source=%s)\n",
		renderSteps(plan.compile), args.CompileTask, plan.compile.Provenance)
	fmt.Fprintf(&b, "  test command: %s (%s, source=%s)%s\n",
		renderSteps(plan.test), args.TestTask, plan.test.Provenance, targetNote(plan.target, plan.test))
	return b.String()
}

// targetNote says the scope the test command will ACTUALLY run at, never the
// one that was merely requested. A resolver note means the target did not land —
// a composite slot such as verify has no single command for one to fall into, so
// test_task:"verify" + test_target ran the whole suite while the header claimed
// a scope. In a report whose entire value is knowing which tests ran, a false
// scope line is worse than none.
func targetNote(target string, cmd TaskCommand) string {
	if len(cmd.Notes) > 0 {
		return " — " + strings.Join(cmd.Notes, " ")
	}
	if target == "" {
		return " — WHOLE suite; pass test_target to scope it (topology_affected says to what)"
	}
	return " — scoped to " + target
}

func renderSteps(cmd TaskCommand) string {
	parts := make([]string, 0, len(cmd.Steps))
	for _, argv := range cmd.Steps {
		parts = append(parts, strings.Join(argv, " "))
	}
	return strings.Join(parts, " && ")
}

// formatMutationResult renders one mutant: its verdict, the edit it made, the
// timing of each step, and — for the outcomes where evidence matters — an
// excerpt of the output that produced the verdict.
func formatMutationResult(n int, r mutationResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%d] %-8s %s%s\n", n, strings.ToUpper(string(r.outcome)), r.display, labelNote(r.spec.Label))
	fmt.Fprintf(&b, "    %s → %s\n", quoteMutantText(r.spec.Old), quoteMutantText(r.spec.New))
	b.WriteString(mutationVerdictLines(r))
	b.WriteString("\n")
	return b.String()
}

// mutationVerdictLines is the per-outcome body: the timing line plus whatever
// evidence that verdict rests on.
func mutationVerdictLines(r mutationResult) string {
	var b strings.Builder
	if !r.compile.ran {
		fmt.Fprintf(&b, "    ✗ INVALID — %s. NOT A KILL: nothing was compiled or tested.\n", r.reason)
		return b.String()
	}
	fmt.Fprintf(&b, "    %s · %s\n", stepLine("compile", r.compile), stepLine("tests", r.test))
	switch r.outcome {
	case MutationKilled:
		b.WriteString("    ✓ killed — a test failed, so this line is genuinely asserted.\n")
		b.WriteString(excerpt(r.test.output))
	case MutationSurvived:
		b.WriteString("    ⚠ SURVIVED — the mutant compiled and every test still passed.\n")
		b.WriteString("      The assertions covering this line do not actually test it.\n")
	case MutationInvalid:
		fmt.Fprintf(&b, "    ✗ INVALID — %s. NOT A KILL: the tests never got a fair run.\n", r.reason)
		b.WriteString(excerpt(failingStepOutput(r)))
	}
	return b.String()
}

// failingStepOutput picks the output that explains an invalid verdict: the
// compile gate's when the gate is what failed, the test step's otherwise.
func failingStepOutput(r mutationResult) string {
	if r.compile.timedOut || r.compile.exitCode != 0 {
		return r.compile.output
	}
	return r.test.output
}

func stepLine(name string, s stepOutcome) string {
	switch {
	case !s.ran:
		return name + " not run"
	case s.timedOut:
		return fmt.Sprintf("%s TIMED OUT (%s)", name, s.elapsed.Round(time.Millisecond))
	case s.exitCode == 0:
		return fmt.Sprintf("%s ok (%s)", name, s.elapsed.Round(time.Millisecond))
	default:
		return fmt.Sprintf("%s exit %d (%s)", name, s.exitCode, s.elapsed.Round(time.Millisecond))
	}
}

func labelNote(label string) string {
	if label == "" {
		return ""
	}
	return "  — " + label
}

// quoteMutantText renders a mutant side compactly: newlines become ⏎ so a
// multi-line edit stays on one line, and an empty new_string is named rather
// than rendered as `""`, which reads as a bug.
func quoteMutantText(s string) string {
	if s == "" {
		return "(deleted)"
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", " ⏎ ")
	return `"` + textfmt.Ellipsis(s, 90) + `"`
}

// excerpt renders the TAIL of a step's output, indented as a quote block. The
// tail, not the head: a test runner's verdict and the assertion that produced
// it land at the end, while the head is setup noise.
func excerpt(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	trimmed := false
	if len(lines) > mutationExcerptLines {
		lines = lines[len(lines)-mutationExcerptLines:]
		trimmed = true
	}
	var b strings.Builder
	if trimmed {
		b.WriteString("      | …\n")
	}
	for _, l := range lines {
		fmt.Fprintf(&b, "      | %s\n", l)
	}
	return b.String()
}

// mutationSummary is the closing block: the counts, then the actionable
// findings, then the restoration statement.
func mutationSummary(results []mutationResult) string {
	var killed, survived, invalid int
	for _, r := range results {
		switch r.outcome {
		case MutationKilled:
			killed++
		case MutationSurvived:
			survived++
		case MutationInvalid:
			invalid++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "summary: %d killed · %d survived · %d invalid (of %d %s)\n",
		killed, survived, invalid, len(results), textfmt.Plural(len(results), "mutant", "mutants"))
	if survived > 0 {
		fmt.Fprintf(&b, "⚠ %d %s SURVIVED — the tests pass with the code changed, so those assertions are vacuous. Fix the test, not the mutant.\n",
			survived, textfmt.Plural(survived, "mutant", "mutants"))
	}
	if invalid > 0 {
		fmt.Fprintf(&b, "⚠ %d %s INVALID — an invalid mutant proves NOTHING and is not a kill. Correct it and re-run before trusting the assertion.\n",
			invalid, textfmt.Plural(invalid, "mutant", "mutants"))
	}
	if survived == 0 && invalid == 0 {
		b.WriteString("✓ every mutant was killed by a real test failure, each one compiled first.\n")
	}
	b.WriteString("✓ every file restored and verified against its pre-run sha256.\n")
	return b.String()
}
