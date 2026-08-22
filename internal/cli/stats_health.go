package cli

// stats_health.go — `plumb stats --health` (PLAN-368): renders the three
// standing health metrics for one UTC day and persists them into
// stats.db's health_daily table, so the same command doubles as the
// nightly-computable job the card asks for — a cron entry (or an overnight
// agent) can just run it once a day, and re-running it for the same day is
// safe (health_daily is upserted, not appended to).

import (
	"fmt"
	"strconv"
	"time"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/tui"
)

var statsFlagHealth bool

func init() {
	statsCmd.Flags().BoolVar(&statsFlagHealth, "health", false, "show the three standing health metrics for today (UTC) instead of the default view — lane-defection rate, semantic-surface error rate, net economics")
}

// runStatsHealth computes and renders today's (UTC) health metrics, and
// upserts them into health_daily. It opens the DB read-write (not
// OpenReadOnly, unlike the rest of `plumb stats`): the first run against an
// older stats.db must be able to apply the health_daily migration, and every
// run needs to persist rows.
func runStatsHealth(ws string) error {
	PrintLogo()

	db, err := stats.Open()
	if err != nil {
		return fmt.Errorf("opening stats db: %w", err)
	}
	defer db.Close()

	tui.RebuildStyles()
	day := time.Now().UTC()

	laneDefection, err := db.LaneDefectionForDay(day, ws)
	if err != nil {
		return fmt.Errorf("computing lane-defection rate: %w", err)
	}
	semantic, err := db.SemanticErrorRatesSince(day.Add(-7*24*time.Hour), day.Add(24*time.Hour), ws, tools.IsPinned, stats.DefaultSemanticBaselineMultiplier)
	if err != nil {
		return fmt.Errorf("computing semantic error rates: %w", err)
	}
	economics, err := db.EconomicsForDay(day, ws)
	if err != nil {
		return fmt.Errorf("computing net economics: %w", err)
	}

	if err := persistHealthDaily(db, ws, laneDefection, semantic, economics); err != nil {
		return fmt.Errorf("persisting health_daily: %w", err)
	}

	fmt.Println(render.ContextBox(
		fmt.Sprintf("%s\n%s", render.ContractPath(ws), tui.MutedStyle.Render("Health metrics for "+laneDefection.Day+" (UTC)")),
		tui.SepStyle,
	))
	fmt.Println()

	renderLaneDefection(laneDefection)
	renderSemanticErrorRates(semantic)
	renderNetEconomics(economics)
	return nil
}

// persistHealthDaily upserts one health_daily row per computed metric —
// idempotent per day: re-running for the same day overwrites these same
// rows rather than accumulating duplicates (UpsertHealthDaily's ON CONFLICT).
func persistHealthDaily(db *stats.DB, ws string, ld stats.LaneDefectionDay, semantic []stats.SemanticErrorRate, econ []stats.EconomicsDay) error {
	if err := db.UpsertHealthDaily(stats.HealthDailyRow{
		Day: ld.Day, Workspace: ws, Metric: stats.MetricLaneDefection,
		SessionsTotal: ld.SessionsTotal, SessionsFlagged: ld.SessionsFlagged,
	}); err != nil {
		return err
	}
	for _, s := range semantic {
		if err := db.UpsertHealthDaily(stats.HealthDailyRow{
			Day: ld.Day, Workspace: ws, Metric: stats.MetricSemanticError, Tool: s.Tool,
			CallsTotal: s.Calls, CallsErrors: s.Errors, Advertised: s.Advertised, Flagged: s.Flagged,
		}); err != nil {
			return err
		}
	}
	for _, e := range econ {
		if err := db.UpsertHealthDaily(stats.HealthDailyRow{
			Day: ld.Day, Workspace: ws, ClientName: e.ClientName, Metric: stats.MetricNetEconomics,
			SavingsTokens: e.SavingsTokens, GuardRefusals: e.GuardRefusals,
		}); err != nil {
			return err
		}
	}
	return nil
}

func renderLaneDefection(ld stats.LaneDefectionDay) {
	fmt.Println("Lane-defection rate")
	fmt.Println(tui.HintStyle.Render(
		"  share of sessions where a plumb-read file was subsequently modified on disk by something other\n" +
			"  than plumb, detected via the same guard that refuses a stale write (KindUnreadOrStale). This is\n" +
			"  an APPROXIMATION that undercounts: it only catches a defection the session later tried to write\n" +
			"  over. Trend matters, not the absolute."))
	if ld.SessionsTotal == 0 {
		fmt.Println("  no sessions recorded today")
	} else {
		fmt.Printf("  %d / %d sessions flagged (%.1f%%)\n", ld.SessionsFlagged, ld.SessionsTotal, ld.Rate()*100)
	}
	fmt.Println()
}

func renderSemanticErrorRates(rates []stats.SemanticErrorRate) {
	fmt.Println("Semantic-surface error rate (7-day rolling)")
	fmt.Println(tui.HintStyle.Render(
		"  per-tool error rate across the LSP query/edit surface, since this is what's least used and least\n" +
			"  reliable (worth-it strategy §1). `advertised` = pinned into a Claude Code connection's context on\n" +
			"  every call (tools.PinnedTools, PLAN-355). `flagged` = advertised AND rate > read_file's own rate ×\n" +
			fmt.Sprintf("  %.0f (configurable).", stats.DefaultSemanticBaselineMultiplier)))
	if len(rates) == 0 {
		fmt.Println("  no semantic-tool calls in the last 7 days")
		fmt.Println()
		return
	}
	t := render.DottedTableBase(tui.SepStyle, tui.HintStyle).
		Headers("Tool", "Calls", "Errors", "Rate", "Advertised", "Flagged")
	for _, s := range rates {
		t.Row(s.Tool, strconv.FormatInt(s.Calls, 10), strconv.FormatInt(s.Errors, 10),
			fmt.Sprintf("%.1f%%", s.Rate()*100), boolCell(s.Advertised), boolCell(s.Flagged))
	}
	fmt.Println(t.Render())
	fmt.Println()
}

func renderNetEconomics(econ []stats.EconomicsDay) {
	fmt.Println("Net economics per client (trended daily)")
	fmt.Println(tui.HintStyle.Render(
		"  two of PLAN-367's three economics lines, netted to the current savings-model version only: the\n" +
			"  estimated read savings and the guard-refusal count. The third line (profile tool-schema surcharge)\n" +
			"  is a live per-connection figure `plumb stats` itself cannot reconstruct after the fact, so it is\n" +
			"  not trended here either — see internal/stats/health_economics.go."))
	if len(econ) == 0 {
		fmt.Println("  no calls recorded today")
		fmt.Println()
		return
	}
	t := render.DottedTableBase(tui.SepStyle, tui.HintStyle).
		Headers("Client", "Estimated savings (tokens)", "Guard refusals")
	for _, e := range econ {
		client := e.ClientName
		if client == "" {
			client = "(unknown)"
		}
		t.Row(client, "~"+stats.FormatSavings(int(e.SavingsTokens)), strconv.FormatInt(e.GuardRefusals, 10))
	}
	fmt.Println(t.Render())
	fmt.Println()
}

func boolCell(b bool) string {
	if b {
		return "yes"
	}
	return "—"
}
