package stats

// health.go — the health_daily table: three standing metrics computed
// idempotently per day so a regression in plumb's own value proposition
// shows up in days, not a year later by anecdote (PLAN-368, the worth-it
// strategy's W2-14 — "agents stopped using plumb" was discovered a year
// after the stats already showed it).
//
// One additive table backs all three metrics, keyed so a re-run for a day
// already computed is an upsert, not a duplicate: (day, workspace,
// client_name, metric, tool). See health_lane_defection.go,
// health_semantic.go and health_economics.go for how each metric is
// computed; this file only owns the table shape and its read/write path.
//
// Every metric here is a COUNT or a RATIO over what `calls` actually
// recorded — never a duration (db.go's "don't trust duration columns" note
// applies here too) — and every one is documented, here and in the CLI
// render, as an approximation: the trend matters, not the absolute.

import (
	"fmt"
	"time"
)

// healthDailyDDL is the health_daily table shape (schema v18). Additive
// only — no existing column changes, per the stats-schema trap this card
// names. WITHOUT ROWID: the natural key IS the primary key, same choice
// db.go made for read_tracking in internal/sessionstate.
const healthDailyDDL = `CREATE TABLE IF NOT EXISTS health_daily (
    day               TEXT    NOT NULL,
    workspace         TEXT    NOT NULL DEFAULT '',
    client_name       TEXT    NOT NULL DEFAULT '',
    metric            TEXT    NOT NULL,
    tool              TEXT    NOT NULL DEFAULT '',
    sessions_total    INTEGER NOT NULL DEFAULT 0,
    sessions_flagged  INTEGER NOT NULL DEFAULT 0,
    calls_total       INTEGER NOT NULL DEFAULT 0,
    calls_errors      INTEGER NOT NULL DEFAULT 0,
    advertised        INTEGER NOT NULL DEFAULT 0,
    flagged           INTEGER NOT NULL DEFAULT 0,
    savings_tokens    INTEGER NOT NULL DEFAULT 0,
    guard_refusals    INTEGER NOT NULL DEFAULT 0,
    computed_at       INTEGER NOT NULL,
    PRIMARY KEY (day, workspace, client_name, metric, tool)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_hd_day ON health_daily(day);`

// HealthMetric names which of the three standing metrics a health_daily row
// belongs to.
type HealthMetric string

const (
	MetricLaneDefection HealthMetric = "lane_defection"
	MetricSemanticError HealthMetric = "semantic_error"
	MetricNetEconomics  HealthMetric = "net_economics"
)

// HealthDailyRow is one persisted row of health_daily. Field use varies by
// Metric: SessionsTotal/SessionsFlagged are lane_defection's; Tool/
// CallsTotal/CallsErrors/Advertised/Flagged are semantic_error's;
// SavingsTokens/GuardRefusals are net_economics'. A field the metric does
// not use is left at its zero value.
type HealthDailyRow struct {
	Day             string // YYYY-MM-DD, UTC
	Workspace       string
	ClientName      string
	Metric          HealthMetric
	Tool            string // semantic_error only; blank otherwise
	SessionsTotal   int64
	SessionsFlagged int64
	CallsTotal      int64
	CallsErrors     int64
	Advertised      bool
	Flagged         bool
	SavingsTokens   int64
	GuardRefusals   int64
}

// UpsertHealthDaily writes one health_daily row, replacing any prior row for
// the same (day, workspace, client_name, metric, tool) key. A re-run for a
// day already computed overwrites rather than duplicating — that is what
// makes the computation idempotent per day.
func (d *DB) UpsertHealthDaily(row HealthDailyRow) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT INTO health_daily
		    (day, workspace, client_name, metric, tool, sessions_total, sessions_flagged,
		     calls_total, calls_errors, advertised, flagged, savings_tokens, guard_refusals, computed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, workspace, client_name, metric, tool) DO UPDATE SET
		    sessions_total=excluded.sessions_total, sessions_flagged=excluded.sessions_flagged,
		    calls_total=excluded.calls_total, calls_errors=excluded.calls_errors,
		    advertised=excluded.advertised, flagged=excluded.flagged,
		    savings_tokens=excluded.savings_tokens, guard_refusals=excluded.guard_refusals,
		    computed_at=excluded.computed_at`,
		row.Day, row.Workspace, row.ClientName, string(row.Metric), row.Tool,
		row.SessionsTotal, row.SessionsFlagged, row.CallsTotal, row.CallsErrors,
		boolToInt(row.Advertised), boolToInt(row.Flagged), row.SavingsTokens, row.GuardRefusals,
		time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("stats: upsert health_daily: %w", err)
	}
	return nil
}

// HealthDailyRows returns every health_daily row for day (YYYY-MM-DD, UTC),
// optionally narrowed to workspace (blank = every workspace).
func (d *DB) HealthDailyRows(day, workspace string) ([]HealthDailyRow, error) {
	if d == nil {
		return nil, nil
	}
	q := `SELECT day, workspace, client_name, metric, tool, sessions_total, sessions_flagged,
	             calls_total, calls_errors, advertised, flagged, savings_tokens, guard_refusals
	      FROM health_daily WHERE day = ?`
	args := []any{day}
	if workspace != "" {
		q += " AND workspace = ?"
		args = append(args, workspace)
	}
	q += " ORDER BY metric, client_name, tool"
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats: health_daily rows: %w", err)
	}
	defer rows.Close()
	var out []HealthDailyRow
	for rows.Next() {
		var r HealthDailyRow
		var metric string
		var advertised, flagged int
		if err := rows.Scan(&r.Day, &r.Workspace, &r.ClientName, &metric, &r.Tool,
			&r.SessionsTotal, &r.SessionsFlagged, &r.CallsTotal, &r.CallsErrors,
			&advertised, &flagged, &r.SavingsTokens, &r.GuardRefusals); err != nil {
			return nil, fmt.Errorf("stats: health_daily scan: %w", err)
		}
		r.Metric = HealthMetric(metric)
		r.Advertised = advertised != 0
		r.Flagged = flagged != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// utcDayBounds returns the [from, to) UnixMilli bounds of day's UTC calendar
// date — shared by every metric in this package that computes "for one day".
func utcDayBounds(day time.Time) (from, to int64) {
	d := day.UTC()
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	return start.UnixMilli(), start.AddDate(0, 0, 1).UnixMilli()
}

// dayWhere builds a "WHERE called_at >= ? AND called_at < ? [AND workspace = ?]"
// clause plus its bind args. Self-contained rather than routed through
// Filter.where() (db_query.go), which has no notion of an upper time bound
// and is concurrently touched by an unrelated analyzer-warning cleanup —
// this keeps the health metrics' SQL surface independent of that file.
func dayWhere(from, to int64, workspace string) (string, []any) {
	where := " WHERE called_at >= ? AND called_at < ?"
	args := []any{from, to}
	if workspace != "" {
		where += " AND workspace = ?"
		args = append(args, workspace)
	}
	return where, args
}
