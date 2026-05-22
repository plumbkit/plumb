package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/golimpio/plumb/internal/config"
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
	if len(items) != 15 {
		t.Fatalf("len(items) = %d, want 15", len(items))
	}
	if items[0].key != skTheme || items[0].kind != settingPopup || !items[0].live {
		t.Errorf("first item should be the live Theme popup, got %+v", items[0])
	}
	if items[0].value != ActiveThemeName {
		t.Errorf("Theme value = %q, want live ActiveThemeName %q", items[0].value, ActiveThemeName)
	}

	byKey := map[settingKey]settingItem{}
	for _, it := range items {
		byKey[it.key] = it
	}
	if ll := byKey[skLogLevel]; !ll.live || ll.restart {
		t.Errorf("log level should be live and not restart-gated, got %+v", ll)
	}
	for _, k := range []settingKey{
		skLogFormat, skStrict, skShowWriteDiff, skRateLimit, skTopology, skQuality,
		skGitWrites, skGitDestructive, skGitPush, skCacheTTL, skCacheMaxSize, skLSPTimeout, skAutoAttach,
	} {
		if !byKey[k].restart {
			t.Errorf("setting %v should be marked restart", k)
		}
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
	if len(lines) == 0 || lines[0].kind != slBlank {
		t.Fatalf("first logical line should be a blank, got %+v", lines)
	}
	// Every settings item must appear exactly once as an slRow.
	seen := map[int]bool{}
	for _, ln := range lines {
		if ln.kind == slRow {
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

func TestSettingsToggle_PersistsAndMarksRestart(t *testing.T) {
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
	if m.settingsStatus == "" {
		t.Error("status should be set after a toggle")
	}
	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Edits.Strict {
		t.Error("toggle should have persisted Strict=true to disk")
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

	// Down past the end clamps to the last index.
	for range 50 {
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
