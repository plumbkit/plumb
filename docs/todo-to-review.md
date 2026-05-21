# Plumb — Completed Work (Pending Review)

Completed items awaiting a review pass. Each notes the commit(s) that shipped it and what was actually delivered.

**Once an item has been reviewed, remove it from this file.** Its canonical record lives in `CHANGELOG.md` and the relevant docs (`docs/architecture.md`, `AGENTS.md`, `docs/mcp-tools.md`, …); any unfinished follow-ups belong in `docs/todo.md`. Keeping reviewed entries here would duplicate that documentation and blur what still needs review — so this file should only ever list work that has *not* yet been reviewed.

---

### Theme system — sanitise, multi-theme, picker, persistence (0.7.7)

**Commits:** pending (this batch)

**Delivered:**

1. **Theme struct hardening** — added `ContrastText color.Color` (drives text rendered on coloured backgrounds; `"0"` for dark themes, `"15"` for light) and `ChromaStyle string` (chroma style for `plumb config show`) to `Theme`. Replaced two hardcoded `lipgloss.Color("0")` calls in `styles.go` with `ActiveTheme.ContrastText`.

2. **Inline style elimination** — four inline `lipgloss.NewStyle()` calls in `model_right.go`'s `rightTabBar()` moved to exported vars (`RightTabActiveLabel`, `RightTabActiveBracket`, `RightTabInactive`, `RightTabMuted`) in `styles.go`, rebuilt in `RebuildStyles()`. Same for `msgStyle` → `WarningMsgStyle` in `model_render.go` and `labelStyle` → `DashLabelStyle` in `dashboard_widgets.go`.

3. **Five new built-in themes** — `darcula`, `dracula`, `gruvbox`, `github-light`, `solarized-light`. Total 6 themes. `isLightTheme()` heuristic classifies themes for the picker badge using SelectionBackground luminance.

4. **Config persistence** — `UIConfig{Theme string}` added to `Config`; default `nordico`. `SaveTheme(name string) error` reads current config, sets `UI.Theme`, re-encodes full TOML (user comments lost on first save — known v1 limitation). `plumb` CLI wires saved theme before `tui.Run()`.

5. **Live theme picker** — Settings section (index 4) now renders a two-panel theme picker: left panel lists all themes with `❯` cursor and `✓` saved marker; right panel shows colour swatches (`██` per token) and a mini preview. Navigation applies the theme live (`ActiveTheme` + `RebuildStyles()`); `esc` reverts; `enter`/`space` saves via `SaveTheme()`.

6. **Dynamic chroma style** — `plumb config show` reads `cfg.UI.Theme` and uses the matching `ChromaStyle` instead of the hardcoded `"nord"`.

7. **Tests** — `TestTheme_AllFieldsSet` (reflect-based nil/empty check on every theme), `TestThemeNames_Sorted`, `TestTheme_AllSixThemesRegistered`, `TestIsLightTheme_Classification`, `TestSaveTheme_RoundTrip`, `TestDefaults_UIThemeIsNordico`.

**Files changed:** `internal/tui/theme.go`, `internal/tui/styles.go`, `internal/tui/model_right.go`, `internal/tui/model_render.go`, `internal/tui/dashboard_widgets.go`, `internal/tui/model_core.go`, `internal/tui/model_keys.go`, `internal/tui/model_update.go`, `internal/tui/model_left.go`, `internal/tui/model_settings.go` (new), `internal/tui/theme_test.go` (new), `internal/config/config.go`, `internal/config/config_test.go`, `internal/cli/root.go`, `internal/cli/config.go`.
