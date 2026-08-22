package stats

// health_economics.go — PLAN-368's third standing metric: PLAN-367's
// economics figures, trended per day per client.
//
// Deliberately TWO of PLAN-367's three lines, not three. The profile
// surcharge line is a LIVE per-connection figure — internal/tools/
// session_start_savings.go computes it from srv.ToolSchemaBytes() /
// srv.ToolFilter, an active mcp.Server's advertised tool set for THIS
// connection — and `plumb stats` itself already declines to show it for
// exactly that reason (internal/cli/stats.go: "computed from a LIVE MCP
// connection's advertised tool set, which this offline stats reader has no
// access to"). A nightly job reading `calls` after the fact has no more
// access to that live state than `plumb stats` does, so trending a
// surcharge figure here would mean fabricating one for a day nobody
// actually measured it on. Net economics here trends what `calls` really
// recorded: the savings estimate and the guard-refusal count.

import (
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/clientcaps"
)

// EconomicsDay is one client's savings/guard-refusal figures for one UTC day.
type EconomicsDay struct {
	Day           string
	Workspace     string
	ClientName    string
	SavingsTokens int64 // model clientcaps.ModelVersion only; never summed across versions
	GuardRefusals int64
}

// EconomicsForDay computes, per distinct client_name seen that UTC day, the
// current-model-version savings total and the guard-refusal count — the two
// of PLAN-367's three economics lines that `calls` can still answer after
// the fact (see the file doc comment for why the third, the schema
// surcharge, is not here).
func (d *DB) EconomicsForDay(day time.Time, workspace string) ([]EconomicsDay, error) {
	if d == nil {
		return nil, nil
	}
	from, to := utcDayBounds(day)
	where, args := dayWhere(from, to, workspace)

	clients, err := d.distinctClients(where, args)
	if err != nil {
		return nil, err
	}

	dayStr := day.UTC().Format("2006-01-02")
	out := make([]EconomicsDay, 0, len(clients))
	for _, client := range clients {
		clientWhere := where + " AND client_name = ?"
		clientArgs := append(append([]any{}, args...), client)

		savings, err := d.sumSavingsTokens(clientWhere, clientArgs)
		if err != nil {
			return nil, err
		}
		refusals, err := d.countGuardRefusals(clientWhere, clientArgs)
		if err != nil {
			return nil, err
		}
		out = append(out, EconomicsDay{
			Day: dayStr, Workspace: workspace, ClientName: client,
			SavingsTokens: savings, GuardRefusals: refusals,
		})
	}
	return out, nil
}

func (d *DB) distinctClients(where string, args []any) ([]string, error) {
	//nolint:gosec // G202: where is dayWhere's ? placeholders only
	q := "SELECT DISTINCT client_name FROM tool_calls" + where + " ORDER BY client_name"
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats: distinct clients: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("stats: distinct clients scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// sumSavingsTokens sums capability+efficiency tokens, netted to
// clientcaps.ModelVersion exactly as SummarySinceVersion nets a per-tool row
// (db_query_versioned.go) — summing an unversioned total here would repeat
// the exact mistake PLAN-367 review round 1 fixed.
func (d *DB) sumSavingsTokens(where string, args []any) (int64, error) {
	//nolint:gosec // G202: where is dayWhere's ? placeholders only; the model version travels as a bound arg
	q := `SELECT COALESCE(SUM(CASE WHEN savings_model_version = ? THEN capability_tokens + efficiency_tokens ELSE 0 END), 0)
	      FROM tool_calls` + where
	var total int64
	if err := d.db.QueryRow(q, append([]any{clientcaps.ModelVersion}, args...)...).Scan(&total); err != nil {
		return 0, fmt.Errorf("stats: sum savings tokens: %w", err)
	}
	return total, nil
}

// countGuardRefusals counts failed calls classified under one of
// preventedIncidentKinds (db_failures.go) — the same "guard refusals"
// PreventedIncidents counts for the session_start banner and `plumb stats`,
// just bounded to one day/client here instead of routed through Filter.
func (d *DB) countGuardRefusals(where string, args []any) (int64, error) {
	placeholders := make([]string, len(preventedIncidentKinds))
	kindArgs := make([]any, len(preventedIncidentKinds))
	for i, k := range preventedIncidentKinds {
		placeholders[i] = "?"
		kindArgs[i] = string(k)
	}
	//nolint:gosec // G202: where is dayWhere's ? placeholders only; the IN(...) list is fixed ? placeholders too
	q := "SELECT COUNT(*) FROM tool_calls" + where + " AND success = 0 AND error_kind IN (" + strings.Join(placeholders, ",") + ")"
	var n int64
	if err := d.db.QueryRow(q, append(append([]any{}, args...), kindArgs...)...).Scan(&n); err != nil {
		return 0, fmt.Errorf("stats: guard refusals: %w", err)
	}
	return n, nil
}
