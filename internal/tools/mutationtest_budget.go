package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// mutationSilentBudget is how long a run may go without telling its client
// anything. A client that asked for no progress hears nothing until the report,
// and an MCP client commonly abandons a call that has been silent for a while:
// Claude Code does at 30 minutes by default (CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT).
// The budget sits under that with room for the estimate to be wrong. A var only
// so a test can shrink it.
var mutationSilentBudget = 25 * time.Minute

// checkSilentBudget refuses a run that is bound to outlast the client's idle
// window when nothing will keep the call alive. It runs after the baseline,
// which is what gives it a measured per-mutant cost (one compile + test cycle),
// and before any mutant is written, so a refused run leaves every file untouched.
//
// A call carrying a progressToken is not refused here: enterStep reports each
// step at its start and on a heartbeat while it runs, and each report resets
// the client's idle window. Without a token the run would be dropped mid-way,
// its report lost, while it went on holding the slot and writing mutants until
// it finished for nobody.
//
// The baseline is only an ESTIMATE of a mutant's cost, and it can be far too
// low: the unmutated tree's `go test` result may come from Go's test cache, so
// the baseline pays little more than the compile while every mutant (a changed
// file) pays for the whole suite. silentRunWatch re-checks with each mutant's
// real cost as the run goes.
func checkSilentBudget(ctx context.Context, elapsed, perMutant time.Duration, mutants int) error {
	return silentBudgetRefusal(mcp.HasProgress(ctx), elapsed, perMutant, mutants, mutationSilentBudget)
}

// silentRunWatch carries the silent-call budget through the run, so that a
// baseline which understated the cost cannot let a silent call run past the
// client's idle window. Before each mutant it projects the rest of the run from
// the costliest cycle seen so far, the baseline included, and stops the run when
// the projection passes the budget. The report then covers the mutants that ran
// and says why the rest did not. It is inert for a call with progress.
//
// It sees a cycle's cost only after that cycle has run, so a silent run can
// still pass the budget by at most one cycle — the first mutant runs on the
// baseline's estimate alone. With the default 600 s step timeout that is ~21
// minutes in the worst case, inside Claude Code's 30-minute window.
type silentRunWatch struct {
	active bool // the call asked for no progress, so it is silent until the report
	start  time.Time
	budget time.Duration
	worst  time.Duration // costliest compile+test cycle seen
}

func newSilentRunWatch(ctx context.Context, start time.Time, baselineCost time.Duration) *silentRunWatch {
	return &silentRunWatch{active: !mcp.HasProgress(ctx), start: start, budget: mutationSilentBudget, worst: baselineCost}
}

// observe records one mutant's real cycle cost.
func (w *silentRunWatch) observe(cost time.Duration) {
	if w != nil && cost > w.worst {
		w.worst = cost
	}
}

// stopBefore is consulted before the next mutant, with ran mutants done out of
// total. It returns the report note explaining an early stop, or "" to go on.
func (w *silentRunWatch) stopBefore(now time.Time, ran, total int) string {
	remaining := total - ran
	if w == nil || !w.active || remaining <= 0 || w.worst <= 0 {
		return ""
	}
	left := time.Duration(remaining) * w.worst
	projected := now.Sub(w.start) + left
	if projected <= w.budget {
		return ""
	}
	return fmt.Sprintf("\n⚠ stopped early: %d of %d %s never ran. The costliest compile+test cycle so far took %s, so running the rest "+
		"would keep this silent call going for about %s in all, past its %s budget — a client may give up on it and lose this report "+
		"(the unmutated baseline can understate a cycle: Go's test cache can make an unchanged tree's suite look cheap). "+
		"This client did not ask for progress notifications, which would keep the call alive. "+
		"Run the remaining mutants as a smaller batch. They prove nothing either way.\n",
		remaining, total, textfmt.Plural(total, "mutant", "mutants"), roughDuration(w.worst), roughDuration(projected), roughDuration(w.budget))
}

// silentBudgetRefusal is checkSilentBudget's decision, pure for testing.
func silentBudgetRefusal(hasProgress bool, elapsed, perMutant time.Duration, mutants int, budget time.Duration) error {
	if hasProgress || perMutant <= 0 {
		return nil
	}
	estimate := elapsed + time.Duration(mutants)*perMutant
	if estimate <= budget {
		return nil
	}
	fit := int((budget - elapsed) / perMutant)
	advice := fmt.Sprintf("Split the mutants into batches of at most %d, or scope the test with test_target/test_run so each cycle is cheaper", fit)
	if fit < 1 {
		advice = "Not even one mutant fits: scope the test with test_target/test_run so each cycle is cheaper"
	}
	return fmt.Errorf("mutation_test: refused before mutating anything — the unmutated baseline took %s, so %d %s would keep this call silent for about %s, "+
		"past the %s budget for a call with no progress: a client may give up on a silent call (Claude Code does at 30 min) and the report would be lost. "+
		"This client did not ask for progress notifications, which would keep the call alive. %s",
		roughDuration(perMutant), mutants, textfmt.Plural(mutants, "mutant", "mutants"), roughDuration(estimate), roughDuration(budget), advice)
}

// roughDuration renders d to the minute ("1h30m", "4m"), or to the second
// under a minute ("40s"). Unlike humaniseAge it keeps the minutes past an
// hour, which an estimate of 1h30m would otherwise lose a third of.
func roughDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}
