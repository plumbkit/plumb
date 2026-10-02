package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// mutationSilentBudget is how long a run may go without telling its client
// anything. A client that asked for no progress hears nothing until the report,
// and Claude Code abandons a tools/call that has sent neither a response nor
// progress for 30 minutes by default (CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT). The
// budget sits under that with room for the estimate to be wrong. A var only so
// a test can shrink it.
var mutationSilentBudget = 25 * time.Minute

// checkSilentBudget refuses a run that is bound to outlast the client's idle
// window when nothing will keep the call alive. It runs after the baseline,
// which is what gives it a measured per-mutant cost (one compile + test cycle),
// and before any mutant is written, so a refused run leaves every file untouched.
//
// A call carrying a progressToken is never refused here: enterStep reports each
// step, and each report resets the client's idle window. Without one the run
// would be dropped mid-way — its report lost, and the slot held, and mutants
// written, until the run finished for nobody.
func checkSilentBudget(ctx context.Context, elapsed, perMutant time.Duration, mutants int) error {
	if mcp.HasProgress(ctx) || perMutant <= 0 {
		return nil
	}
	estimate := elapsed + time.Duration(mutants)*perMutant
	if estimate <= mutationSilentBudget {
		return nil
	}
	fit := int((mutationSilentBudget - elapsed) / perMutant)
	advice := fmt.Sprintf("Split the mutants into batches of at most %d, or scope the test with test_target/test_run so each cycle is cheaper", fit)
	if fit < 1 {
		advice = "Not even one mutant fits: scope the test with test_target/test_run so each cycle is cheaper"
	}
	return fmt.Errorf("mutation_test: refused before mutating anything — the unmutated baseline took %s, so %d %s would run about %s with no word to the client, "+
		"past the %s a client may wait on a silent call (Claude Code abandons one at 30 min, and the report would be lost). "+
		"This client did not ask for progress notifications, which would keep the call alive. %s",
		perMutant.Round(time.Second), mutants, textfmt.Plural(mutants, "mutant", "mutants"), estimate.Round(time.Minute), mutationSilentBudget, advice)
}
