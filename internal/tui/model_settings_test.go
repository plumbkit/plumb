package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/config"
)

func cursorFor(items []settingItem, key settingKey) int {
	for i, it := range items {
		if it.key == key {
			return i
		}
	}
	return -1
}

func newSettingsModel() Model {
	cfg := config.Defaults()
	return Model{
		settingsCfg:   cfg,
		settingsItems: buildSettingItems(cfg),
	}
}

func TestBuildSettingItems_ShapeAndFlags(t *testing.T) {
	items := buildSettingItems(config.Defaults())
	// Full coverage: one row per config field (well over the original curated 16).
	if len(items) < 30 {
		t.Fatalf("len(items) = %d, want full coverage (>= 30)", len(items))
	}
	if items[0].key != skTheme || items[0].kind != settingPopup {
		t.Errorf("first item should be the Theme popup, got %+v", items[0])
	}
	if items[0].value != ActiveThemeName {
		t.Errorf("Theme value = %q, want live ActiveThemeName %q", items[0].value, ActiveThemeName)
	}
	// Every row carries a one-line help string for the status bar.
	for _, it := range items {
		if it.help == "" {
			t.Errorf("row %q (key %v) is missing help text", it.label, it.key)
		}
	}
}

// TestReloadTierFor pins each settings key to its reload tier. The reloadRestart
// set must stay in lock-step with the fields config.RestartSensitiveEqual
// compares (log format + cache) — anything else the daemon hot-reloads, so
// marking it restart-needed would mislead the user (the bug this fixes).
func TestReloadTierFor(t *testing.T) {
	// The reloadRestart set must stay exactly {log format, log file, cache} — the
	// only fields the daemon cannot hot-reload. Marking anything else restart-
	// needed would mislead the user.
	restart := map[settingKey]bool{
		skLogFormat: true, skLogFile: true, skCacheTTL: true, skCacheMaxSize: true,
	}
	for _, it := range buildSettingItems(config.Defaults()) {
		tier := reloadTierFor(it.key)
		if restart[it.key] {
			if tier != reloadRestart {
				t.Errorf("reloadTierFor(%v) = %d, want reloadRestart", it.key, tier)
			}
			continue
		}
		if tier == reloadRestart {
			t.Errorf("reloadTierFor(%v) = reloadRestart, but only log format/file + cache may be restart-needed", it.key)
		}
	}
	// Spot-check representative live and next-session keys.
	if reloadTierFor(skStrict) != reloadLive {
		t.Error("strict edits should be reloadLive")
	}
	if reloadTierFor(skQuality) != reloadNextSession {
		t.Error("quality should be reloadNextSession")
	}
}

func TestSettingsGitToggle_Persists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skGitPush)

	// AllowPush defaults to false; toggling should turn it on and persist.
	m, _ = m.adjustSetting(1)

	if !m.settingsCfg.Git.AllowPush {
		t.Error("git allow push should be on after toggle")
	}
	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Git.AllowPush {
		t.Error("toggle should have persisted Git.AllowPush=true to disk")
	}
}

func TestSettingsDurationCycle_Persists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skLSPTimeout)

	start := m.settingsCfg.LSPQuery.Timeout.Duration
	m, _ = m.adjustSetting(1)
	if m.settingsCfg.LSPQuery.Timeout.Duration == start {
		t.Error("lsp query timeout should have changed after a cycle step")
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.LSPQuery.Timeout.Duration != m.settingsCfg.LSPQuery.Timeout.Duration {
		t.Errorf("persisted timeout = %v, want %v", got.LSPQuery.Timeout.Duration, m.settingsCfg.LSPQuery.Timeout.Duration)
	}
}

func TestThemePickerOrder_DarkBeforeLight(t *testing.T) {
	dark, light := themeGroups()
	if len(dark) == 0 || len(light) == 0 {
		t.Fatalf("expected both dark and light themes, got dark=%d light=%d", len(dark), len(light))
	}
	for _, n := range dark {
		if isLightTheme(AvailableThemes[n]) {
			t.Errorf("%q classified dark but isLightTheme is true", n)
		}
	}
	for _, n := range light {
		if !isLightTheme(AvailableThemes[n]) {
			t.Errorf("%q classified light but isLightTheme is false", n)
		}
	}
	order := themePickerOrder()
	if len(order) != len(dark)+len(light) {
		t.Fatalf("order length = %d, want %d", len(order), len(dark)+len(light))
	}
	// All dark names must precede all light names in the navigation order.
	for i, n := range order {
		inDark := i < len(dark)
		if inDark == isLightTheme(AvailableThemes[n]) {
			t.Errorf("order[%d]=%q misplaced relative to the dark/light split", i, n)
		}
	}
}

func TestCycleOption(t *testing.T) {
	opts := []string{"debug", "info", "warn", "error"}
	cases := []struct {
		cur  string
		dir  int
		want string
	}{
		{"info", 1, "warn"},
		{"info", -1, "debug"},
		{"error", 1, "debug"},  // wrap forward
		{"debug", -1, "error"}, // wrap backward
		{"unknown", 1, "info"}, // unknown treated as index 0
	}
	for _, c := range cases {
		if got := cycleOption(opts, c.cur, c.dir); got != c.want {
			t.Errorf("cycleOption(%q, %d) = %q, want %q", c.cur, c.dir, got, c.want)
		}
	}
}

func TestSettingsLogicalLines_GroupsAndRows(t *testing.T) {
	items := buildSettingItems(config.Defaults())
	lines := settingsLogicalLines(items)
	if len(lines) == 0 || lines[0].kind != slHeader {
		t.Fatalf("first logical line should be a group header (no leading blank), got %+v", lines)
	}
	// Every settings item must appear exactly once as a primary slRow (cont==0);
	// list-entry continuation lines (cont>0) legitimately repeat the item index.
	seen := map[int]bool{}
	for _, ln := range lines {
		if ln.kind == slRow && ln.cont == 0 {
			if seen[ln.item] {
				t.Errorf("item %d appears more than once", ln.item)
			}
			seen[ln.item] = true
		}
	}
	if len(seen) != len(items) {
		t.Errorf("rows cover %d items, want %d", len(seen), len(items))
	}
	// Each distinct group contributes exactly one header.
	headers := 0
	for _, ln := range lines {
		if ln.kind == slHeader {
			headers++
		}
	}
	groups := map[string]bool{}
	for _, it := range items {
		groups[it.group] = true
	}
	if headers != len(groups) {
		t.Errorf("header count = %d, want %d (one per group)", headers, len(groups))
	}
}

func TestSelectSettingAtBodyRow_MapsClickToRow(t *testing.T) {
	m := newSettingsModel()
	m.height = 40 // tall enough that nothing scrolls
	m.settingsScroll = 0

	// Logical line 2 is the first row (blank, header, row...). Screen row =
	// bodyStartRow + lineIndex.
	lines := settingsLogicalLines(m.settingsItems)
	firstRowLine := -1
	for i, ln := range lines {
		if ln.kind == slRow {
			firstRowLine = i
			break
		}
	}
	m.selectSettingAtBodyRow(bodyStartRow + firstRowLine)
	if m.settingsCursor != lines[firstRowLine].item {
		t.Errorf("cursor = %d after click, want %d", m.settingsCursor, lines[firstRowLine].item)
	}

	// Clicking a header line should not change the cursor.
	m.settingsCursor = 3
	for i, ln := range lines {
		if ln.kind == slHeader {
			m.selectSettingAtBodyRow(bodyStartRow + i)
			break
		}
	}
	if m.settingsCursor != 3 {
		t.Errorf("clicking a header changed the cursor to %d", m.settingsCursor)
	}
}

func TestSettingsToggle_PersistsAndMarksLive(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skStrict)

	m, _ = m.adjustSetting(1)

	if !m.settingsCfg.Edits.Strict {
		t.Error("Strict should be on after toggle")
	}
	if v := m.settingsItems[cursorFor(m.settingsItems, skStrict)].value; v != "on" {
		t.Errorf("strict row value = %q, want \"on\"", v)
	}
	// Strict edits hot-reload, so the status must say so — not "restart".
	if !strings.Contains(m.settingsStatus, "applied live") {
		t.Errorf("strict status = %q, want it to mention \"applied live\"", m.settingsStatus)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Edits.Strict {
		t.Error("toggle should have persisted Strict=true to disk")
	}
}

// TestSettingsLogFormat_StatusMarksRestart confirms a genuinely restart-bound
// setting still tells the user a restart is needed.
func TestSettingsLogFormat_StatusMarksRestart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skLogFormat)

	m = m.setLogFormat("json")
	if !strings.Contains(m.settingsStatus, "next daemon start") {
		t.Errorf("log format status = %q, want it to mention a daemon restart", m.settingsStatus)
	}
}

// TestSettingsPathStyle_StatusMarksLive confirms path style (a live-tier
// setting) now shows the "applied live" suffix, not a stale blank.
func TestSettingsPathStyle_StatusMarksLive(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skPathStyle)

	m, _ = m.adjustSetting(1)

	if !strings.Contains(m.settingsStatus, "applied live") {
		t.Errorf("path style status = %q, want it to mention \"applied live\"", m.settingsStatus)
	}
	if !strings.Contains(m.settingsStatus, "path style") {
		t.Errorf("path style status = %q, want it to mention \"path style\"", m.settingsStatus)
	}
}

func TestSettingsRateLimit_StepAndFloor(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skRateLimit)

	start := m.settingsCfg.Edits.RateLimitPerMinute
	m, _ = m.adjustSetting(-1)
	if got := m.settingsCfg.Edits.RateLimitPerMinute; got != start-10 {
		t.Errorf("rate limit = %d, want %d", got, start-10)
	}

	// Drive it below zero — it must floor at 0 ("off").
	for range 100 {
		m, _ = m.adjustSetting(-1)
	}
	if got := m.settingsCfg.Edits.RateLimitPerMinute; got != 0 {
		t.Errorf("rate limit floored = %d, want 0", got)
	}
	if v := m.settingsItems[cursorFor(m.settingsItems, skRateLimit)].value; v != "off" {
		t.Errorf("rate limit value = %q, want \"off\"", v)
	}
}

func TestSettingsLogFormat_Cycles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skLogFormat)

	m, _ = m.adjustSetting(1)
	if m.settingsCfg.LogFormat != "json" {
		t.Errorf("log format = %q, want \"json\"", m.settingsCfg.LogFormat)
	}
}

// TestSettingsGenericNumber_Persists exercises the generic setNumber path on a
// newly-added field (topology resync batch), confirming the per-field step and
// the floor at 0.
func TestSettingsGenericNumber_Persists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skTopoResyncBatch)
	start := m.settingsCfg.Topology.ResyncBatch
	m, _ = m.adjustSetting(1)
	if got := m.settingsCfg.Topology.ResyncBatch; got != start+25 {
		t.Errorf("resync batch = %d, want %d", got, start+25)
	}
	for range 100 {
		m, _ = m.adjustSetting(-1)
	}
	if got := m.settingsCfg.Topology.ResyncBatch; got != 0 {
		t.Errorf("resync batch floored = %d, want 0", got)
	}
}

// TestSettingsGenericCycle_Persists exercises the generic setCycle path on the
// quality mode enum.
func TestSettingsGenericCycle_Persists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skQualityMode)
	m, _ = m.adjustSetting(1)
	if m.settingsCfg.Quality.Mode != "sync" {
		t.Errorf("quality mode = %q, want \"sync\"", m.settingsCfg.Quality.Mode)
	}
}

// TestSettingsGenericToggle_Persists exercises a newly-added bool field via the
// shared toggle path (topology watch).
func TestSettingsGenericToggle_Persists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skTopoWatch)
	start := m.settingsCfg.Topology.Watch
	m, _ = m.adjustSetting(1)
	if m.settingsCfg.Topology.Watch == start {
		t.Errorf("topology watch did not toggle from %v", start)
	}
}

// TestSettingsStatusOrHelp_FallsBackToHelp confirms the focused row's help shows
// on the status line when there is no transient action status.
func TestSettingsStatusOrHelp_FallsBackToHelp(t *testing.T) {
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skStrict)
	m.settingsStatus = ""
	if got := m.settingsStatusOrHelp(); got != m.settingsItems[m.settingsCursor].help {
		t.Errorf("settingsStatusOrHelp() = %q, want the row help", got)
	}
	m.settingsStatus = "saved"
	if got := m.settingsStatusOrHelp(); got != "saved" {
		t.Errorf("settingsStatusOrHelp() = %q, want the transient status", got)
	}
}

func TestSettingsNavigation_ClampsAndSkipsNothing(t *testing.T) {
	m := newSettingsModel()
	m.currentSection = 4

	// Up at the top stays at 0.
	m, _ = m.handleSettingsSectionKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if m.settingsCursor != 0 {
		t.Errorf("cursor = %d at top, want 0", m.settingsCursor)
	}

	// Down moves to 1.
	m, _ = m.handleSettingsSectionKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if m.settingsCursor != 1 {
		t.Errorf("cursor = %d after down, want 1", m.settingsCursor)
	}

	// Down past the end clamps to the last index (press more times than there are
	// rows so the count of rows can grow without breaking this test).
	for range len(m.settingsItems) + 5 {
		m, _ = m.handleSettingsSectionKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	}
	if want := len(m.settingsItems) - 1; m.settingsCursor != want {
		t.Errorf("cursor = %d at bottom, want %d", m.settingsCursor, want)
	}
}

func TestSettingsEnterOpensThemePicker(t *testing.T) {
	m := newSettingsModel()
	m.currentSection = 4
	m.settingsCursor = cursorFor(m.settingsItems, skTheme)

	m, _ = m.handleSettingsSectionKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if !m.showThemePicker {
		t.Error("enter on the Theme row should open the theme picker")
	}
}

func TestThemePickerRow_Format(t *testing.T) {
	origName := ActiveThemeName
	t.Cleanup(func() { ActiveThemeName = origName })
	ActiveThemeName = "nordico"

	if got := themePickerRow("nordico", true); got != "❯ nordico ✓" {
		t.Errorf("focused active row = %q, want \"❯ nordico ✓\"", got)
	}
	if got := themePickerRow("darcula", false); got != "  darcula" {
		t.Errorf("unfocused inactive row = %q, want \"  darcula\"", got)
	}
}

func TestMaybeOpenThemePicker_GlobalShortcut(t *testing.T) {
	// ^t opens the picker from a non-Settings section.
	m := newSettingsModel()
	m.currentSection = 0
	got, _ := m.handleKeyMsg(ctrlKey('t'))
	if !got.showThemePicker {
		t.Error("ctrl+t should open the theme picker from any section")
	}

	// It is ignored while another overlay (help) is showing.
	m2 := newSettingsModel()
	m2.showHelp = true
	got2, _ := m2.maybeOpenThemePicker()
	if got2.showThemePicker {
		t.Error("ctrl+t should not open the picker while help is showing")
	}
}

func TestThemePicker_MoveAppliesAndSaves(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	origTheme, origName := ActiveTheme, ActiveThemeName
	t.Cleanup(func() {
		ActiveTheme, ActiveThemeName = origTheme, origName
		RebuildStyles()
	})

	names := themePickerOrder()
	m := newSettingsModel()
	m.showThemePicker = true
	m.themePickerCursor = 0
	ActiveThemeName = names[0]

	m, _ = m.handleThemePickerKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if m.themePickerCursor != 1 {
		t.Fatalf("cursor = %d after down, want 1", m.themePickerCursor)
	}
	if ActiveThemeName != names[1] {
		t.Errorf("ActiveThemeName = %q, want %q (applied live)", ActiveThemeName, names[1])
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.UI.Theme != names[1] {
		t.Errorf("saved theme = %q, want %q", got.UI.Theme, names[1])
	}

	// esc closes without reverting.
	m, _ = m.handleThemePickerKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	if m.showThemePicker {
		t.Error("esc should close the theme picker")
	}
	if ActiveThemeName != names[1] {
		t.Error("esc should not revert the applied theme")
	}
}
