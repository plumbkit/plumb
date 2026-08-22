package stats

// health_semantic.go — PLAN-368's second standing metric: a 7-day rolling
// error rate per tool on plumb's LSP-driven "semantic surface" (the LSP
// queries and LSP edits from AGENTS.md's own tool index — get_definition,
// find_references, rename_symbol, and their siblings), with an `advertised`
// flag reporting whether a tool's schema is pinned into a Claude Code
// connection's context on every call (tools.PinnedTools, PLAN-355) rather
// than reachable only through a ToolSearch round-trip.
//
// An advertised tool is FLAGGED once its error rate crosses read_file's own
// rate — the cheapest, most-exercised tool in the registry, so its rate is
// the honest "what does normal look like" baseline — times a configurable
// multiplier. This is deliberately the shape of the incident the strategy
// doc names: an advertised semantic tool sliding to a double-digit error
// rate while read_file stayed under 2%, discovered by anecdote a year later.

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// SemanticTools is the LSP-driven tool surface this metric tracks: the LSP
// queries and LSP edits from AGENTS.md's own tool index. It is deliberately
// NOT clientcaps' catSemantic list (internal/clientcaps/score.go) — that one
// answers a savings-model question (which tools have a defensible token
// counterfactual) and omits the symbol-EDIT tools entirely. This one answers
// a correctness question, so it also tracks rename_symbol,
// replace_symbol_body and their siblings — exactly where the strategy doc's
// cited error rates (rename_symbol 17.2%, replace_symbol_body 16.9%,
// safe_delete_symbol 50%) were found.
var SemanticTools = map[string]bool{
	"workspace_symbols":    true,
	"get_definition":       true,
	"explain_symbol":       true,
	"file_outline":         true,
	"find_references":      true,
	"call_hierarchy":       true,
	"type_hierarchy":       true,
	"diagnostics":          true,
	"read_symbol":          true,
	"rename_symbol":        true,
	"replace_symbol_body":  true,
	"insert_before_symbol": true,
	"insert_after_symbol":  true,
	"safe_delete_symbol":   true,
	"move_symbol":          true,
}

// DefaultSemanticBaselineMultiplier is the default threshold: an advertised
// semantic tool is flagged once its rate crosses read_file's own rate times
// this. The card names the multiplier explicitly as configurable —
// SemanticErrorRatesSince's multiplier parameter.
const DefaultSemanticBaselineMultiplier = 3.0

// SemanticErrorRate is one tool's rolling error rate over some window.
type SemanticErrorRate struct {
	Tool       string
	Calls      int64
	Errors     int64
	Advertised bool
	Baseline   float64 // read_file's rate over the same window × multiplier
	Flagged    bool    // Advertised && Rate() > Baseline
}

// Rate is Errors / Calls, or 0 when the tool had no calls in the window.
func (s SemanticErrorRate) Rate() float64 {
	if s.Calls == 0 {
		return 0
	}
	return float64(s.Errors) / float64(s.Calls)
}

// SemanticErrorRatesSince computes the per-tool error rate over
// [since, until) for every SemanticTools member that had at least one call
// in the window. advertised reports whether a tool's schema is pinned into
// context on every Claude Code connection (tools.IsPinned, PLAN-355) —
// injected rather than imported, since internal/tools sits above
// internal/stats in the layered architecture (internal/arch) and cannot be
// imported from here. multiplier <= 0 uses DefaultSemanticBaselineMultiplier.
// Results are sorted by tool name for a stable, diffable render.
func (d *DB) SemanticErrorRatesSince(since, until time.Time, workspace string, advertised func(tool string) bool, multiplier float64) ([]SemanticErrorRate, error) {
	if d == nil {
		return nil, nil
	}
	if multiplier <= 0 {
		multiplier = DefaultSemanticBaselineMultiplier
	}
	where, args := dayWhere(since.UnixMilli(), until.UnixMilli(), workspace)

	baselineCalls, baselineErrors, err := d.toolCallsAndErrors(where, args, "read_file")
	if err != nil {
		return nil, err
	}
	var baselineRate float64
	if baselineCalls > 0 {
		baselineRate = float64(baselineErrors) / float64(baselineCalls)
	}
	baseline := baselineRate * multiplier

	tools := make([]string, 0, len(SemanticTools))
	for tool := range SemanticTools {
		tools = append(tools, tool)
	}
	sort.Strings(tools)

	var out []SemanticErrorRate
	for _, tool := range tools {
		calls, errs, err := d.toolCallsAndErrors(where, args, tool)
		if err != nil {
			return nil, err
		}
		if calls == 0 {
			continue
		}
		isAdvertised := advertised != nil && advertised(tool)
		s := SemanticErrorRate{
			Tool: tool, Calls: calls, Errors: errs,
			Advertised: isAdvertised, Baseline: baseline,
		}
		s.Flagged = isAdvertised && s.Rate() > baseline
		out = append(out, s)
	}
	return out, nil
}

func (d *DB) toolCallsAndErrors(where string, args []any, tool string) (calls, failed int64, err error) {
	//nolint:gosec // G202: where is dayWhere's ? placeholders only; tool travels as a bound arg
	q := "SELECT COUNT(*), SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END) FROM tool_calls" + where + " AND tool = ?"
	var errN sql.NullInt64
	if scanErr := d.db.QueryRow(q, append(append([]any{}, args...), tool)...).Scan(&calls, &errN); scanErr != nil {
		return 0, 0, fmt.Errorf("stats: semantic error rate for %s: %w", tool, scanErr)
	}
	return calls, errN.Int64, nil
}
