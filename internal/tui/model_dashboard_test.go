// Dashboard tests: the activity sparkline and call formatting, the segment and
// token-savings bars, and every dash* widget's layout and alert behaviour.
// Split out of model_test.go, which had grown past the test-file size cap.
package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/monitor"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestProjectConversationSummaries_CrossProjectRequiresWorkspaceConsent(t *testing.T) {
	ws := t.TempDir()
	global, err := collab.OpenGlobalAt(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = global.Close() })
	now := time.Now()
	conv, err := global.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "alice", AuthorID: "sess-alice", Body: "cross", Addressee: "bob", TargetID: "sess-bob",
		TTL: time.Hour, OriginWorkspace: "/other", TargetWorkspace: ws,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	base := config.Defaults()
	if projectAllowsCrossProject(base, ws) {
		t.Fatal("default workspace unexpectedly opted into cross-project metadata")
	}
	if got := projectConversationSummaries(context.Background(), ws, now, global, false); len(got) != 0 {
		t.Fatalf("TUI exposed cross-project metadata without consent: %#v", got)
	}

	base.Collab.CrossProject = true
	if !projectAllowsCrossProject(base, ws) {
		t.Fatal("resolved workspace lost explicit cross-project consent")
	}
	if got := projectConversationSummaries(context.Background(), "/other", now, global, true); len(got) != 0 {
		t.Fatalf("sender workspace exposed recipient-scoped metadata: %#v", got)
	}
	got := projectConversationSummaries(context.Background(), ws, now, global, true)
	if len(got) != 1 || got[0].ID != conv || got[0].Notes != 1 || got[0].Pending != 1 {
		t.Fatalf("TUI omitted opted-in cross-project metadata: %#v", got)
	}
}

func TestLiveProjectAllowsCrossProject_RefreshesAndFailsClosed(t *testing.T) {
	ws := t.TempDir()
	cfg := config.Defaults()
	cfg.Collab.CrossProject = true
	load := func() (config.Config, error) { return cfg, nil }
	if !liveProjectAllowsCrossProject(ws, load) {
		t.Fatal("live consent loader lost enabled policy")
	}
	cfg.Collab.CrossProject = false
	if liveProjectAllowsCrossProject(ws, load) {
		t.Fatal("live consent loader retained revoked policy")
	}
	if liveProjectAllowsCrossProject(ws, func() (config.Config, error) {
		return config.Config{}, errors.New("unreadable")
	}) {
		t.Fatal("unreadable live config did not fail closed")
	}
}

func TestActivitySparklineAndCallFormatting(t *testing.T) {
	got := activitySparkline([]int64{0, 1, 2, 4}, 4)
	if got != " ⡀⡄⡇" {
		t.Fatalf("activitySparkline = %q, want %q", got, " ⡀⡄⡇")
	}
	rendered := ansiStripForTest(renderActivityGraph(got, SelectedStyle, SepStyle))
	if rendered != "⣀⡀⡄⡇" {
		t.Fatalf("renderActivityGraph = %q, want faded dot baseline", rendered)
	}
	blank := ansiStripForTest(renderActivityGraph(activitySparkline(nil, 4), SelectedStyle, SepStyle))
	if blank != "⣀⣀⣀⣀" {
		t.Fatalf("blank activity graph = %q, want faded baseline dots", blank)
	}
	for n, want := range map[int64]string{
		0:       "0 calls",
		1:       "1 call",
		999:     "999 calls",
		5200:    "5.2k calls",
		1200000: "1.2m calls",
	} {
		if got := formatActivityCalls(n); got != want {
			t.Fatalf("formatActivityCalls(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		45 * time.Second:          "< 1m",
		12 * time.Minute:          "12m+",
		3*time.Hour + time.Minute: "3h+",
		11 * 24 * time.Hour:       "11d+",
		45 * 24 * time.Hour:       "1mo 15d+",
		18 * 30 * 24 * time.Hour:  "1y 6mo+",
	} {
		if got := formatUptimePrecise(d); got != want {
			t.Fatalf("formatUptimePrecise(%s) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[int64]string{
		0:       "0",
		1:       "1",
		123:     "123",
		999:     "999",
		1000:    "1k",
		1200:    "1.2k",
		5200:    "5.2k",
		999000:  "999k",
		1000000: "1m",
		1200000: "1.2m",
	} {
		if got := formatActivityCount(n); got != want {
			t.Fatalf("formatActivityCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestCPUSparklineUsesFixedPercentScale(t *testing.T) {
	got := cpuSparkline([]float64{0, 25, 50, 75, 100, 150}, 6)
	if got != " ⡄⡇⣧⣿⣿" {
		t.Fatalf("cpuSparkline = %q, want fixed 0-100%% scale", got)
	}
	if got := cpuSparkline(nil, 4); got != "    " {
		t.Fatalf("cpuSparkline(nil) = %q, want blank sparkline", got)
	}
}

func TestPercentSegmentBar(t *testing.T) {
	if filled, unfilled := percentSegmentBar(42.5, 20); filled != "■■■■■■■■" || unfilled != "■■■■■■■■■■■■" {
		t.Fatalf("percentSegmentBar = %q+%q, want 8 filled and 12 unfilled", filled, unfilled)
	}
	if filled, unfilled := percentSegmentBar(0.1, 20); filled != "■" || unfilled != "■■■■■■■■■■■■■■■■■■■" {
		t.Fatalf("percentSegmentBar tiny percent = %q+%q, want one visible segment", filled, unfilled)
	}
	if filled, unfilled := percentSegmentBar(0, 4); filled != "" || unfilled != "■■■■" {
		t.Fatalf("percentSegmentBar zero = %q+%q, want empty bar", filled, unfilled)
	}
}

func TestTokenSavingsBar(t *testing.T) {
	if filled, unfilled := tokenSavingsBar(913000, 12); filled != "■■■■■■■" || unfilled != "■■■■■" {
		t.Fatalf("tokenSavingsBar = %q+%q, want sample shape", filled, unfilled)
	}
	if filled, unfilled := tokenSavingsBar(0, 4); filled != "" || unfilled != "■■■■" {
		t.Fatalf("tokenSavingsBar(0) = %q+%q, want empty bar", filled, unfilled)
	}
}

func TestSplitGroups(t *testing.T) {
	tests := []struct {
		name             string
		cap, eff         int64
		groups           int
		wantCap, wantEff int
	}{
		{"zero total", 0, 0, 8, 0, 0},
		{"zero groups", 100, 100, 0, 0, 0},
		{"even split", 100, 100, 8, 4, 4},
		{"all capability", 500, 0, 8, 8, 0},
		{"all efficiency", 0, 500, 8, 0, 8},
		{"tiny capability sliver", 166, 27_897_000, 8, 1, 7},
		{"tiny efficiency sliver", 27_897_000, 166, 8, 7, 1},
		{"rounds up to six", 3, 1, 8, 6, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCap, gotEff := splitGroups(tt.cap, tt.eff, tt.groups)
			if gotCap != tt.wantCap || gotEff != tt.wantEff {
				t.Fatalf("splitGroups(%d,%d,%d) = (%d,%d), want (%d,%d)", tt.cap, tt.eff, tt.groups, gotCap, gotEff, tt.wantCap, tt.wantEff)
			}
			if tt.groups > 0 && tt.cap+tt.eff > 0 && gotCap+gotEff != tt.groups {
				t.Fatalf("split does not sum to groups: %d+%d != %d", gotCap, gotEff, tt.groups)
			}
		})
	}
}

func TestTokenSavingsSplitRow(t *testing.T) {
	RebuildStyles()
	const groups = 8

	row := tokenSavingsSplitRow(stats.AxisTotals{Capability: 166, Efficiency: 27_897_000}, groups)
	if got := strings.Count(ansiStripForTest(row), "▆▆"); got != groups {
		t.Fatalf("split row column count = %d, want %d", got, groups)
	}
	if got := strings.Count(row, OkStyle.Render("▆▆")); got != 1 {
		t.Fatalf("capability columns = %d, want 1", got)
	}
	if got := strings.Count(row, SelectedStyle.Render("▆▆")); got != groups-1 {
		t.Fatalf("efficiency columns = %d, want %d", got, groups-1)
	}

	// No savings recorded yet: every column is the dim separator style, none lit.
	empty := tokenSavingsSplitRow(stats.AxisTotals{}, groups)
	if got := strings.Count(ansiStripForTest(empty), "▆▆"); got != groups {
		t.Fatalf("empty split row column count = %d, want %d", got, groups)
	}
	if got := strings.Count(empty, SepStyle.Render("▆▆")); got != groups {
		t.Fatalf("empty split row dim columns = %d, want %d", got, groups)
	}
	if strings.Contains(empty, OkStyle.Render("▆▆")) || strings.Contains(empty, SelectedStyle.Render("▆▆")) {
		t.Fatalf("empty split row should have no lit columns")
	}
}

func TestDashDaemonWidgetUsesMemoryRows(t *testing.T) {
	RebuildStyles()
	m := Model{
		daemonMetricsOK: true,
		daemonMetrics: monitor.DaemonMetrics{
			PID:            38247,
			RSSAvailable:   true,
			RSSBytes:       33 * 1024 * 1024,
			HeapAllocBytes: 5 * 1024 * 1024,
			HeapInuseBytes: 9 * 1024 * 1024,
			HeapSysBytes:   15 * 1024 * 1024,
			NumGC:          20,
			Goroutines:     20,
			CPUAvailable:   true,
			CPUPercent:     99,
		},
	}

	lines := m.dashDaemonWidget(dashDaemonMinInner)
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
	}

	if !strings.Contains(plain[0], "Daemon Memory (PID 38247)") {
		t.Fatalf("daemon title = %q, want memory title with pid", plain[0])
	}
	for _, line := range plain {
		if strings.Contains(line, "CPU") || strings.Contains(line, "99") {
			t.Fatalf("daemon memory widget should not render CPU data: %q", line)
		}
	}
	assertRow := func(idx int, label, value string) {
		t.Helper()
		if !strings.Contains(plain[idx], label) || !strings.Contains(plain[idx], value) {
			t.Fatalf("daemon row %d = %q, want label %q and value %q", idx, plain[idx], label, value)
		}
	}
	// RSS is the current sample, not a peak — the label must not claim otherwise.
	assertRow(2, "RSS", "33 MiB")
	if strings.Contains(plain[2], "Peak") {
		t.Fatalf("daemon RSS row = %q, should not be labelled Peak (it is the current sample)", plain[2])
	}
	assertRow(4, "Heap In Use", "9 MiB")
	assertRow(5, "Heap Sys", "15 MiB")
	assertRow(6, "Heap Released", "0 B")
	assertRow(7, "GC", "20 cycles")
}

func TestDashboardDaemonVersionAlert(t *testing.T) {
	oldVersion := Version
	Version = "0.7.0"
	defer func() { Version = oldVersion }()

	m := Model{sessions: []session.Info{{DaemonVersion: "0.6.9"}}}
	got := m.dashboardDaemonVersionAlert()
	if !strings.Contains(got, "running 0.6.9") || !strings.Contains(got, "run plumb restart") {
		t.Fatalf("dashboardDaemonVersionAlert = %q, want mismatch with restart hint", got)
	}
}

func TestDashboardWorkspaceStateAlert(t *testing.T) {
	m := Model{sessions: []session.Info{{Synthetic: true}}}
	if got := m.dashboardWorkspaceStateAlert(); !strings.Contains(got, "auto-attached") {
		t.Fatalf("dashboardWorkspaceStateAlert synthetic = %q, want auto-attached alert", got)
	}

	m = Model{
		dashProjectFolder: "/repo",
		sessions:          []session.Info{{Folder: "/repo", Language: "none"}},
	}
	if got := m.dashboardWorkspaceStateAlert(); !strings.Contains(got, "LSP unavailable") {
		t.Fatalf("dashboardWorkspaceStateAlert language none = %q, want LSP alert", got)
	}
}

func TestDashboardErrorSpikeAlert(t *testing.T) {
	m := Model{dashUptimeTopTools: []stats.ToolStat{
		{Tool: "read_file", Calls: 8, Errors: 2},
		{Tool: "edit_file", Calls: 4, Errors: 1},
	}}
	got := m.dashboardErrorSpikeAlert()
	if !strings.Contains(got, "3/12") {
		t.Fatalf("dashboardErrorSpikeAlert = %q, want 3/12 failure summary", got)
	}

	m = Model{dashUptimeTopTools: []stats.ToolStat{{Tool: "read_file", Calls: 20, Errors: 2}}}
	if got := m.dashboardErrorSpikeAlert(); got != "" {
		t.Fatalf("dashboardErrorSpikeAlert below threshold = %q, want empty", got)
	}
}

func TestDashTokensWidgetUsesLargeTwoColumnLayout(t *testing.T) {
	RebuildStyles()
	m := Model{
		activity: stats.ActivitySummary{
			Window: 12 * time.Minute,
		},
		dashLifetimeFirstAt: time.Now().Add(-9 * 24 * time.Hour),
		dashLifetimeTokens:  518000,
		dashLifetimeAxes:    stats.AxisTotals{Efficiency: 518000},
	}

	lines := m.dashTokensWidget(dashTokensMinInner)
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
	}

	wantBlock := "│   ▆▆ ▆▆ ▆▆ ▆▆ ▆▆ ▆▆ ▆▆ ▆▆   ▆▆ ▆▆ ▆▆ ▆▆ ▆▆ ▆▆ ▆▆ ▆▆   │"
	for _, idx := range []int{2, 3, 4, 5} {
		if plain[idx] != wantBlock {
			t.Fatalf("tokens block row %d = %q, want %q", idx, plain[idx], wantBlock)
		}
	}
	wantLabels := "│   uptime 0 (12m+)           cap ~0 · eff ~518k (9d+)"
	if !strings.HasPrefix(plain[7], wantLabels) {
		t.Fatalf("tokens label row = %q, want prefix %q", plain[7], wantLabels)
	}
	if len(plain) > 8 && strings.Contains(plain[8], "eff ~") {
		t.Fatalf("caption should be one line, got a second axis line: %q", plain[8])
	}
}

func TestDashTopToolsTablesRenderWidgets(t *testing.T) {
	RebuildStyles()
	m := Model{
		dashLifetimeTopTools: []stats.ToolStat{
			{Tool: "diagnostics", Calls: 1800, AvgMs: 0, P95Ms: 0, TokensSaved: 442000, EfficiencyTokens: 442000},
		},
		dashUptimeTopTools: []stats.ToolStat{
			{Tool: "read_file", Calls: 12, AvgMs: 1, P95Ms: 2, Errors: 1},
		},
	}

	tableLines := m.dashTopToolsTables(80)
	plain := make([]string, 0, len(tableLines))
	for _, line := range tableLines {
		plain = append(plain, ansiStripForTest(line))
	}
	if !strings.Contains(plain[0], "Top Tools (all time)") || !strings.HasPrefix(plain[0], "╭─") {
		t.Fatalf("all-time widget top = %q, want framed title", plain[0])
	}
	if !strings.Contains(plain[2], "Tool") || !strings.Contains(plain[2], "Calls") || !strings.Contains(plain[2], "Efficiency") {
		t.Fatalf("all-time header = %q, want compact columns", plain[2])
	}
	if strings.Contains(plain[2], "Avg ms") || strings.Contains(plain[2], "P95 ms") || strings.Contains(plain[2], "Errors") {
		t.Fatalf("all-time header kept dense table columns: %q", plain[2])
	}
	if !strings.Contains(plain[4], "diagnostics") || !strings.Contains(plain[4], "1.8k") || !strings.Contains(plain[4], "~442k") {
		t.Fatalf("all-time row = %q, want diagnostics calls and savings", plain[4])
	}
	callsHeaderEnd := strings.Index(plain[2], "Calls") + len("Calls")
	callsValueEnd := strings.Index(plain[4], "1.8k") + len("1.8k")
	if callsValueEnd != callsHeaderEnd {
		t.Fatalf("calls column is not right-aligned with header:\n%s\n%s", plain[2], plain[4])
	}
	if !strings.Contains(plain[8], "Top Tools (uptime)") || !strings.HasPrefix(plain[8], "╭─") {
		t.Fatalf("uptime widget top = %q, want second framed widget", plain[8])
	}
	if !strings.Contains(plain[10], "Tool") || !strings.Contains(plain[10], "Calls") || !strings.Contains(plain[10], "Errors") {
		t.Fatalf("uptime header = %q, want compact current-state columns", plain[10])
	}
	if !strings.Contains(plain[12], "read_file") || !strings.Contains(plain[12], "12") || !strings.Contains(plain[12], "1") {
		t.Fatalf("uptime row = %q, want read_file calls and errors", plain[12])
	}
}

func TestDashTopToolsTablesRenderSideBySideWhenWide(t *testing.T) {
	RebuildStyles()
	m := Model{
		dashLifetimeTopTools: []stats.ToolStat{{Tool: "diagnostics", Calls: 1800, TokensSaved: 442000, EfficiencyTokens: 442000}},
		dashUptimeTopTools:   []stats.ToolStat{{Tool: "read_file", Calls: 12, Errors: 1}},
	}

	plainTop := ansiStripForTest(m.dashTopToolsTables(140)[0])
	if !strings.Contains(plainTop, "Top Tools (all time)") || !strings.Contains(plainTop, "╮   ╭─ Top Tools (uptime)") {
		t.Fatalf("wide top tools should render side by side with three-space gap:\n%s", plainTop)
	}
}

func TestDashTopToolsTablesEqualFrameHeightSideBySide(t *testing.T) {
	RebuildStyles()
	// All-time has many tools; uptime has one — the uptime frame must be padded to match.
	lifetime := make([]stats.ToolStat, 8)
	for i := range lifetime {
		lifetime[i] = stats.ToolStat{Tool: fmt.Sprintf("tool_%d", i), Calls: int64(100 - i), TokensSaved: 1000, EfficiencyTokens: 1000}
	}
	m := Model{
		dashLifetimeTopTools: lifetime,
		dashUptimeTopTools:   []stats.ToolStat{{Tool: "read_file", Calls: 12, Errors: 1}},
	}

	lines := m.dashTopToolsTables(140)
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
	}

	// Both frames must close on the final line, and neither earlier.
	last := len(plain) - 1
	if got := strings.Count(plain[last], "╰"); got != 2 {
		t.Fatalf("final line should close both frames (want 2 '╰', got %d):\n%s", got, plain[last])
	}
	for i := range last {
		if strings.Contains(plain[i], "╰") {
			t.Fatalf("a frame closed early at line %d, frames are not equal height:\n%s", i, strings.Join(plain, "\n"))
		}
	}
}

func TestDashProjectWidgetRendersTopToolsTableInsideWidget(t *testing.T) {
	RebuildStyles()
	m := Model{
		dashProjectFolder:    "plumb",
		dashLifetimeSessions: 8,
		dashLifetimeCalls:    200,
		dashLifetimeTokens:   64000,
		dashProjectSessions:  4,
		dashProjectCalls:     120,
		dashProjectTokens:    32000,
		dashProjectTopTools: []stats.ToolStat{
			{Tool: "read_file", Calls: 12, AvgMs: 1, P95Ms: 2, TokensSaved: 4800, CapabilityTokens: 4800},
		},
		dashProjectConversations: []collab.ConversationSummary{
			{ID: "c123", Notes: 14, Pending: 2},
		},
	}

	lines := m.dashProjectWidget(90)
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansiStripForTest(line)
	}
	if !strings.Contains(plain[0], "Project: plumb") {
		t.Fatalf("project title = %q, want project title", plain[0])
	}
	if !strings.Contains(plain[2], "Sessions") || !strings.Contains(plain[2], "4 (50%)") || !strings.Contains(plain[2], "■") {
		t.Fatalf("project sessions ratio row = %q, want proportional summary", plain[2])
	}
	if !strings.Contains(plain[4], "Efficiency") || !strings.Contains(plain[4], "~32k (50%)") || !strings.Contains(plain[4], "■") {
		t.Fatalf("project efficiency ratio row = %q, want proportional summary", plain[4])
	}
	if !strings.Contains(plain[6], "Top Tools") || !strings.Contains(plain[6], "Efficiency") {
		t.Fatalf("project top tools header = %q, want embedded table header", plain[6])
	}
	if !strings.Contains(plain[8], "read_file") || !strings.Contains(plain[8], "~4.8k") {
		t.Fatalf("project top tools row = %q, want project tool stats", plain[8])
	}
	callsHeaderEnd := strings.Index(plain[6], "Calls") + len("Calls")
	callsValueEnd := strings.Index(plain[8], "12") + len("12")
	joined := strings.Join(plain, "\n")
	if !strings.Contains(joined, "Notes by conversation") ||
		!strings.Contains(joined, "c123  14 notes · 2 pending") {
		t.Fatalf("conversation volume missing from project widget:\n%s", joined)
	}
	if callsValueEnd != callsHeaderEnd {
		t.Fatalf("project calls column is not right-aligned with header:\n%s\n%s", plain[6], plain[8])
	}
}

func TestDashTopToolsTablesHideEmptyUptime(t *testing.T) {
	RebuildStyles()
	m := Model{
		dashLifetimeTopTools: []stats.ToolStat{{Tool: "diagnostics", Calls: 1}},
		dashUptimeTopTools:   []stats.ToolStat{{Tool: "read_file"}},
	}

	plain := strings.Join(m.dashTopToolsTables(80), "\n")
	plain = ansiStripForTest(plain)
	if strings.Contains(plain, "Top Tools (uptime)") {
		t.Fatalf("empty uptime table should be hidden:\n%s", plain)
	}
}

func TestDashDaemonWidgetExpandsLeaderWithWidth(t *testing.T) {
	RebuildStyles()
	m := Model{
		daemonMetricsOK: true,
		daemonMetrics: monitor.DaemonMetrics{
			PID:            38247,
			RSSAvailable:   true,
			RSSBytes:       33 * 1024 * 1024,
			HeapAllocBytes: 5 * 1024 * 1024,
			HeapInuseBytes: 9 * 1024 * 1024,
			HeapSysBytes:   15 * 1024 * 1024,
			NumGC:          20,
			Goroutines:     20,
		},
	}

	plainMin := ansiStripForTest(m.dashDaemonWidget(dashDaemonMinInner)[2])
	plainWide := ansiStripForTest(m.dashDaemonWidget(dashDaemonMinInner + 2)[2])
	if strings.Count(plainWide, "⣀") != strings.Count(plainMin, "⣀")+2 {
		t.Fatalf("wide daemon widget did not add leader cells:\nmin:  %s\nwide: %s", plainMin, plainWide)
	}
}

func TestDashTokensWidgetExpandsBlockGroupsWithWidth(t *testing.T) {
	RebuildStyles()
	m := Model{dashLifetimeTokens: 518000}

	plainMin := ansiStripForTest(m.dashTokensWidget(dashTokensMinInner)[2])
	plainWide := ansiStripForTest(m.dashTokensWidget(dashTokensMinInner + 12)[2])
	if strings.Count(plainWide, "▆▆") <= strings.Count(plainMin, "▆▆") {
		t.Fatalf("wide token widget did not add block groups:\nmin:  %s\nwide: %s", plainMin, plainWide)
	}
}

func TestDashStatsRowAllocatesTokenRemainderToDaemon(t *testing.T) {
	RebuildStyles()
	m := Model{
		dashLifetimeTokens: 518000,
		daemonMetricsOK:    true,
		daemonMetrics: monitor.DaemonMetrics{
			PID:            38247,
			RSSAvailable:   true,
			RSSBytes:       33 * 1024 * 1024,
			HeapAllocBytes: 5 * 1024 * 1024,
			HeapInuseBytes: 9 * 1024 * 1024,
			HeapSysBytes:   15 * 1024 * 1024,
			NumGC:          20,
			Goroutines:     20,
		},
	}
	minRowW := dashDaemonMinInner + 2 + dashWidgetGap + dashTokensMinInner + 2

	row := ansiStripForTest(m.dashStatsRow(minRowW + 2)[2])
	runes := []rune(row)
	daemonOuterW := dashDaemonMinInner + 2 + 2
	if len(runes) < daemonOuterW+dashWidgetGap {
		t.Fatalf("dashboard stats row too short: %q", row)
	}
	daemonPart := string(runes[:daemonOuterW])
	tokenPart := string(runes[daemonOuterW+dashWidgetGap:])
	if got := strings.Count(daemonPart, "⣀"); got != strings.Count(ansiStripForTest(m.dashDaemonWidget(dashDaemonMinInner)[2]), "⣀")+2 {
		t.Fatalf("daemon widget did not absorb token remainder: %q", daemonPart)
	}
	if got := strings.Count(tokenPart, "▆▆"); got != 16 {
		t.Fatalf("token widget should stay at minimum groups for two-column remainder, got %d blocks in %q", got, tokenPart)
	}
}

func TestDashStatsRowUsesThreeSpaceWidgetGap(t *testing.T) {
	RebuildStyles()
	m := Model{
		activity: stats.ActivitySummary{
			Window: 12 * time.Minute,
		},
		dashLifetimeFirstAt: time.Now().Add(-9 * 24 * time.Hour),
		dashLifetimeTokens:  518000,
		daemonMetricsOK:     true,
		daemonMetrics: monitor.DaemonMetrics{
			PID:            38247,
			RSSAvailable:   true,
			RSSBytes:       33 * 1024 * 1024,
			HeapAllocBytes: 5 * 1024 * 1024,
			HeapInuseBytes: 9 * 1024 * 1024,
			HeapSysBytes:   15 * 1024 * 1024,
			NumGC:          20,
			Goroutines:     20,
		},
	}

	plainTop := ansiStripForTest(m.dashStatsRow(120)[0])
	if !strings.Contains(plainTop, "╮   ╭─ Token Efficiency") {
		t.Fatalf("dashboard stats row = %q, want three-space widget gap", plainTop)
	}
}

func TestFormatFriendlySinceDate(t *testing.T) {
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.Local)
	if got := formatFriendlySinceDate(time.Date(2026, time.May, 11, 0, 0, 0, 0, time.Local), now); got != "11 May" {
		t.Fatalf("formatFriendlySinceDate same year = %q, want 11 May", got)
	}
	if got := formatFriendlySinceDate(time.Date(2025, time.May, 11, 0, 0, 0, 0, time.Local), now); got != "11 May 2025" {
		t.Fatalf("formatFriendlySinceDate prior year = %q, want 11 May 2025", got)
	}
}

func TestDashActivityWidgetFramesCaptionsInBorder(t *testing.T) {
	RebuildStyles()
	now := time.Now()
	first := now.AddDate(0, 0, -9)
	m := Model{
		activity: stats.ActivitySummary{
			Calls:  281,
			Window: 2 * time.Hour,
		},
		sessions: []session.Info{
			{ID: "sess-1"},
			{ID: "sess-2"},
		},
		dashLifetimeCalls:    4200,
		dashLifetimeSessions: 96,
		dashLifetimeFirstAt:  first,
	}

	lines := m.dashActivityWidget(120)
	plainTop := ansiStripForTest(lines[0])
	plainBottom := ansiStripForTest(lines[len(lines)-1])
	wantTop := "↓ 4.2k calls (since " + formatFriendlySinceDate(first, now) + ") · 96 sessions"
	if !strings.Contains(plainTop, wantTop) || !strings.HasPrefix(plainTop, "╭─") || !strings.HasSuffix(plainTop, "─╮") {
		t.Fatalf("dashboard activity top border missing %q:\n%s", wantTop, plainTop)
	}
	if strings.Index(plainTop, "since ") > strings.Index(plainTop, "sessions") {
		t.Fatalf("dashboard since date is still on the right side:\n%s", plainTop)
	}
	wantBottom := "↑ 281 calls (uptime) · 2 active sessions"
	if !strings.Contains(plainBottom, wantBottom) || !strings.Contains(plainBottom, "2h+") || !strings.HasPrefix(plainBottom, "╰─") || !strings.HasSuffix(plainBottom, "─╯") {
		t.Fatalf("dashboard activity bottom border missing uptime caption:\n%s", plainBottom)
	}
	for _, idx := range []int{2, 3, 4, 5} {
		plain := ansiStripForTest(lines[idx])
		if !strings.HasPrefix(plain, "│   ") || !strings.HasSuffix(plain, "│") {
			t.Fatalf("chart body line %d is not padded inside widget border: %q", idx, plain)
		}
	}
}

func keyPress(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Text: s, Code: []rune(s)[0]})
}

func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: r, Mod: tea.ModCtrl})
}
