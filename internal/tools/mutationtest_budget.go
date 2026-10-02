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
// step, and each report resets the client's idle window. That covers the gaps
// BETWEEN steps only — a single step longer than the window can still be
// dropped, which the per-step timeout (default 600 s) normally rules out.
// Without a token the run would be dropped mid-way, its report lost, while it
// went on holding the slot and writing mutants until it finished for nobody.
func checkSilentBudget(ctx context.Context, elapsed, perMutant time.Duration, mutants int) error {
	return silentBudgetRefusal(mcp.HasProgress(ctx), elapsed, perMutant, mutants, mutationSilentBudget)
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
