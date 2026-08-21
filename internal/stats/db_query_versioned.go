package stats

// db_query_versioned.go — the savings-model-version-netted per-tool summary
// (PLAN-367 review round 1), split out from db_query.go so the file-size cap
// has room.

import (
	"fmt"
	"log/slog"
	"time"
)

// SummarySinceVersion is Summary with the savings axes (CapabilityTokens,
// EfficiencyTokens, TokensSaved) NETTED to rows scored under exactly
// modelVersion — while Calls/AvgMs/P95Ms/Errors/bytes still reflect every row
// matching filter, whatever savings-model version it was scored under (or
// none at all).
//
// This split matters because every tool_calls row is stamped with the
// CURRENT savings-model version at record time (internal/cli/conn_subsystems.go),
// not just rows a savings tool produced — so a plain Summary(filter) call
// count is honest across a model bump, but a plain Summary(filter) axis total
// is not: it silently sums a v4 row's honest zero next to a v3 row that still
// credited a plain ranged read, understating the drop as though it were real
// usage decline rather than a scoring-model change (PLAN-367 review round 1).
// A `plumb stats` table that put the versioned total in its header but the
// unversioned total in its per-tool rows would show the exact contradiction
// this card exists to remove — this is the fix for that.
func (d *DB) SummarySinceVersion(filter Filter, modelVersion int) ([]ToolStat, error) {
	if d == nil {
		return nil, nil
	}
	where, whereArgs := filter.where()
	// summaryVersionedBase is a compile-time constant; where is built by
	// filter.where() using ? placeholders only — no user values interpolated.
	summaryVersionedBase := `SELECT tool,
		         COUNT(*) AS calls,
		         COALESCE(AVG(duration_ms), 0) AS avg_ms,
		         COALESCE(SUM(input_bytes), 0) AS total_in,
		         COALESCE(SUM(output_bytes), 0) AS total_out,
		         SUM(CASE WHEN success=0 THEN 1 ELSE 0 END) AS errors,
		         MAX(called_at) AS last_called,
		         COALESCE(SUM(CASE WHEN savings_model_version = ? THEN capability_tokens ELSE 0 END), 0) AS cap_tokens,
		         COALESCE(SUM(CASE WHEN savings_model_version = ? THEN efficiency_tokens ELSE 0 END), 0) AS eff_tokens,
		         COALESCE(SUM(CASE WHEN savings_model_version = ? THEN tokens_saved ELSE 0 END), 0) AS tokens_saved
		  FROM tool_calls`
	q := summaryVersionedBase + where + " GROUP BY tool ORDER BY calls DESC" //nolint:gosec // G202: see comment above
	// The three CASE WHEN placeholders appear in the SELECT clause, textually
	// before the WHERE clause's own placeholders — args must be ordered to match.
	args := append([]any{modelVersion, modelVersion, modelVersion}, whereArgs...)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats: summary since version: %w", err)
	}
	defer rows.Close()

	var out []ToolStat
	for rows.Next() {
		var s ToolStat
		var lastMs int64
		var totalIn, totalOut int64
		if err := rows.Scan(&s.Tool, &s.Calls, &s.AvgMs, &totalIn, &totalOut, &s.Errors, &lastMs, &s.CapabilityTokens, &s.EfficiencyTokens, &s.TokensSaved); err != nil {
			slog.Warn("stats: summary since version row scan failed; skipping tool", "err", err)
			continue
		}
		s.TotalInputKB = float64(totalIn) / 1024
		s.TotalOutputKB = float64(totalOut) / 1024
		s.LastCalledAt = time.UnixMilli(lastMs)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	p95map := d.p95All(filter)
	for i := range out {
		out[i].P95Ms = p95map[out[i].Tool]
	}
	return out, nil
}
