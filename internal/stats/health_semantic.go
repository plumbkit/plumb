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
//
// SAMPLE-SIZE GUARD (review round 1). A rate computed from a handful of
// calls is noise, not signal, and the failure mode is exactly backwards from
// what this metric exists to catch: with no baseline calls in the window,
// baselineRate is 0, so ANY single semantic-tool error — one call, one
// failure — crosses "0 × multiplier" and flags. That false-fires hardest in
// the precise regime this metric is meant to detect (agents have gone
// native, so read_file's own call count in the window has also collapsed).
// MinSemanticBaselineCalls and MinSemanticToolCalls gate flagging on both
// sides of the ratio having enough data to mean something; below either
// floor the row reports InsufficientSample/BaselineInsufficient instead of a
// verdict.

import (
	"database/sql"
	"fmt"
	"log/slog"
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
//
// read_symbol is deliberately EXCLUDED (review round 1), unlike every other
// member here: it falls back to a fresh tree-sitter parse when the language
// server is cold or absent (internal/tools/read_symbol.go's
// topologyReadFallback), so it can succeed with the LSP fully down, and it
// can fail for reasons — an ambiguous or unmatched symbol name — that have
// nothing to do with LSP health. Its error rate does not cleanly answer the
// "is the LSP-driven surface degrading" question this metric asks; every
// other tool here has no such fallback; an LSP failure IS its failure.
var SemanticTools = map[string]bool{
	"workspace_symbols":    true,
	"get_definition":       true,
	"explain_symbol":       true,
	"file_outline":         true,
	"find_references":      true,
	"call_hierarchy":       true,
	"type_hierarchy":       true,
	"diagnostics":          true,
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

// MinSemanticBaselineCalls is the minimum number of read_file calls the
// window must contain before its rate is trusted as a baseline. Below this,
// baselineRate is one or two calls away from 0 or 100%, and "0 × multiplier"
// would flag any semantic-tool error at all.
const MinSemanticBaselineCalls = 10

// MinSemanticToolCalls is the minimum number of calls a semantic tool itself
// must have in the window before its OWN rate is trusted enough to flag —
// one error in one call is not a rate.
const MinSemanticToolCalls = 10

// SemanticErrorRate is one tool's rolling error rate over some window.
type SemanticErrorRate struct {
	Tool       string
	Calls      int64
	Errors     int64
	Advertised bool
	// AdvertisedUnknown is true when SemanticErrorRatesSince was called with
	// a nil advertised func — Advertised then reads false for every row, but
	// that false means "not evaluated", not "not pinned". Surfaced here
	// (rather than only logged) so a caller reading the struct sees the
	// distinction, not just a silent false.
	AdvertisedUnknown bool
	Baseline          float64 // read_file's rate over the same window × multiplier
	BaselineCalls     int64   // read_file's call count backing Baseline — the sample-size evidence
	// InsufficientSample is true when this tool's own Calls < MinSemanticToolCalls.
	InsufficientSample bool
	// BaselineInsufficient is true when BaselineCalls < MinSemanticBaselineCalls
	// — Baseline is not trustworthy regardless of this tool's own sample size.
	BaselineInsufficient bool
	// Flagged is Advertised && !InsufficientSample && !BaselineInsufficient &&
	// Rate() > Baseline. Never true when either sample is too small to mean
	// anything, whatever the raw rate says.
	Flagged bool
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
// imported from here. A nil advertised is accepted (every row reads
// Advertised=false, AdvertisedUnknown=true) but is logged loudly — see
// SemanticErrorRate.AdvertisedUnknown — since a caller that forgot to wire it
// would otherwise see a quiet "nothing is ever flagged" with no clue why.
// multiplier <= 0 uses DefaultSemanticBaselineMultiplier. Results are sorted
// by tool name for a stable, diffable render.
func (d *DB) SemanticErrorRatesSince(since, until time.Time, workspace string, advertised func(tool string) bool, multiplier float64) ([]SemanticErrorRate, error) {
	if d == nil {
		return nil, nil
	}
	if multiplier <= 0 {
		multiplier = DefaultSemanticBaselineMultiplier
	}
	if advertised == nil {
		slog.Warn("stats: SemanticErrorRatesSince called with advertised=nil — every row reads Advertised=false " +
			"and nothing can ever flag; pass tools.IsPinned (or an equivalent) unless that is truly intended")
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
	baselineInsufficient := baselineCalls < MinSemanticBaselineCalls

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
			Advertised: isAdvertised, AdvertisedUnknown: advertised == nil,
			Baseline: baseline, BaselineCalls: baselineCalls,
			InsufficientSample: calls < MinSemanticToolCalls, BaselineInsufficient: baselineInsufficient,
		}
		s.Flagged = isAdvertised && !s.InsufficientSample && !baselineInsufficient && s.Rate() > baseline
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
