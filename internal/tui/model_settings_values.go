package tui

// model_settings_values.go — value adjustment for the focused row (←→) and the
// per-field mapper tables (numbers, cycles, toggles, durations) plus the status
// / label helpers.

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/golimpio/plumb/internal/config"
)

// adjustSetting changes the focused row's value by dir (−1 / +1). Dispatch is
// kind-driven so adding a setting only extends the field mappers, not this
// switch (keeping every handler well under gocyclo 15).
func (m Model) adjustSetting(dir int) (Model, tea.Cmd) {
	if m.settingsCursor < 0 || m.settingsCursor >= len(m.settingsItems) {
		return m, nil
	}
	it := m.settingsItems[m.settingsCursor]
	switch it.kind {
	case settingToggle:
		if it.lspLang != "" {
			return m.toggleLSP(it), nil
		}
		return m.toggleBool(it.key, it.value == "on"), nil
	case settingNumber:
		return m.setNumber(it, dir), nil
	case settingCycle:
		return m.adjustCycle(it, dir)
	default:
		return m, nil
	}
}

// adjustCycle routes cycle rows. The global-only cycles (log level applies live,
// log format / path style, the cache/lsp durations) keep their bespoke setters
// and are only ever reached in Global scope; every project-overridable string
// cycle (quality mode) flows through the scope-aware setCycle.
func (m Model) adjustCycle(it settingItem, dir int) (Model, tea.Cmd) {
	switch it.key {
	case skLogLevel:
		return m.setLogLevel(cycleOption(logLevelOptions, m.settingsCfg.LogLevel, dir))
	case skLogFormat:
		return m.setLogFormat(cycleOption(logFormatOptions, m.settingsCfg.LogFormat, dir)), nil
	case skPathStyle:
		return m.setPathStyle(cycleOption(pathStyleOptions, m.settingsCfg.UI.PathStyle, dir)), nil
	case skCacheTTL, skLSPTimeout, skSemTimeout:
		return m.setDuration(it.key, dir), nil
	default:
		return m.setCycle(it, dir), nil
	}
}

// setNumber adjusts a numeric field by its per-field step and persists it in the
// current scope (global config or the workspace's project config). The current
// value is read from the focused row, so it reflects the merged effective value
// in a workspace scope, not the global snapshot.
func (m Model) setNumber(it settingItem, dir int) Model {
	step, label := numberMeta(it.key)
	if it.key == skTopoMaxFileSize { // the only int64 field
		var cur int64
		_, _ = fmt.Sscanf(it.value, "%d", &cur)
		n := cur + int64(dir*step)
		if n < 0 {
			n = 0
		}
		if m.applyScopedSetting(it.key, n, func(c *config.Config) { c.Topology.MaxFileSizeBytes = n }) {
			m.settingsStatus = m.scopedStatus(it.key, fmt.Sprintf("%s → %d", label, n))
		}
		return m
	}
	cur := 0
	if it.value != "off" { // rate limit renders 0 as "off"
		_, _ = fmt.Sscanf(it.value, "%d", &cur)
	}
	n := cur + dir*step
	if n < 0 {
		n = 0
	}
	apply := func(c *config.Config) {
		if p := intField(c, it.key); p != nil {
			*p = n
		}
	}
	if m.applyScopedSetting(it.key, n, apply) {
		m.settingsStatus = m.scopedStatus(it.key, fmt.Sprintf("%s → %d", label, n))
	}
	return m
}

// setCycle cycles a generic string-enum field from its current effective value
// and persists it in the current scope.
func (m Model) setCycle(it settingItem, dir int) Model {
	opts, set, label := cycleMeta(it.key)
	if set == nil {
		return m
	}
	next := cycleOption(opts, it.value, dir)
	if m.applyScopedSetting(it.key, next, func(c *config.Config) { set(c, next) }) {
		m.settingsStatus = m.scopedStatus(it.key, label+" → "+next)
	}
	return m
}

// numberMeta returns the adjust step and status label for a numeric setting.
func numberMeta(key settingKey) (int, string) {
	switch key {
	case skRateLimit:
		return 10, "rate limit"
	case skCacheMaxSize:
		return 100, "cache max_size"
	case skPostWriteDiagMs:
		return 50, "post-write diag (ms)"
	case skConcurrentSkewMs:
		return 25, "concurrent skew (ms)"
	case skTopoMaxFileSize:
		return 65536, "max file size (B)"
	case skTopoResyncBatch:
		return 25, "resync batch"
	case skTopoResyncPauseMs:
		return 5, "resync pause (ms)"
	case skTopoResyncIntervalMin:
		return 5, "resync interval (min)"
	case skQualityTimeoutMs:
		return 500, "quality timeout (ms)"
	case skQualityMaxFindings:
		return 1, "max findings/file"
	case skIdleThresholdMin:
		return 5, "idle threshold (min)"
	case skEvictionTTLMin:
		return 5, "eviction ttl (min)"
	case skSemRerankCandidates:
		return 10, "rerank candidates"
	default:
		return 1, ""
	}
}

// intField returns a pointer to the int config field a numeric row edits
// (excluding the two bespoke ones and the int64 topology cap).
func intField(c *config.Config, key settingKey) *int {
	switch key {
	case skRateLimit:
		return &c.Edits.RateLimitPerMinute
	case skCacheMaxSize:
		return &c.Cache.MaxSize
	case skPostWriteDiagMs:
		return &c.Edits.PostWriteDiagnosticsMs
	case skConcurrentSkewMs:
		return &c.Edits.ConcurrentWriteSkewMs
	case skTopoResyncBatch:
		return &c.Topology.ResyncBatch
	case skTopoResyncPauseMs:
		return &c.Topology.ResyncPauseMs
	case skTopoResyncIntervalMin:
		return &c.Topology.ResyncIntervalMinutes
	case skQualityTimeoutMs:
		return &c.Quality.TimeoutMs
	case skQualityMaxFindings:
		return &c.Quality.MaxFindingsPerFile
	case skIdleThresholdMin:
		return &c.Session.IdleThresholdMinutes
	case skEvictionTTLMin:
		return &c.Session.EvictionTTLMinutes
	case skSemRerankCandidates:
		return &c.Semantics.RerankCandidates
	default:
		return nil
	}
}

// cycleMeta returns the option set, setter, and label for a generic string-enum
// setting. The current value comes from the focused row, so this need not read
// any config snapshot (which would be the global one, wrong in a workspace scope).
func cycleMeta(key settingKey) ([]string, func(*config.Config, string), string) {
	switch key {
	case skQualityMode:
		return qualityModeOptions, func(c *config.Config, v string) { c.Quality.Mode = v }, "quality mode"
	case skSemProvider:
		return config.SemanticsProviders, func(c *config.Config, v string) { c.Semantics.Provider = v }, "provider"
	default:
		return nil, nil, ""
	}
}

func (m Model) setLogFormat(format string) Model {
	if m.persist(func(c *config.Config) { c.LogFormat = format }) {
		m.settingsCfg.LogFormat = format
		m.refreshSettingsItems()
		m.settingsStatus = settingStatus(skLogFormat, "log format → "+format)
	}
	return m
}

// toggleBool flips a boolean setting (cur is the current effective value from
// the focused row) and persists it in the current scope.
func (m Model) toggleBool(key settingKey, cur bool) Model {
	v := !cur
	apply := func(c *config.Config) {
		if p := boolField(c, key); p != nil {
			*p = v
		}
	}
	if m.applyScopedSetting(key, v, apply) {
		m.settingsStatus = m.scopedStatus(key, toggleLabel(key)+" "+onOff(v))
	}
	return m
}

// boolField returns a pointer to the bool config field a toggle row edits.
func boolField(c *config.Config, key settingKey) *bool {
	switch key {
	case skStrict:
		return &c.Edits.Strict
	case skShowWriteDiff:
		return &c.Edits.ShowWriteDiff
	case skTopology:
		return &c.Topology.Enabled
	case skTopoResyncOnAttach:
		return &c.Topology.ResyncOnAttach
	case skTopoWatch:
		return &c.Topology.Watch
	case skQuality:
		return &c.Quality.Enabled
	case skRefuseHomeRoots:
		return &c.Walk.RefuseHomeRoots
	case skAutoAttachPersist:
		return &c.Workspace.AutoAttachPersist
	case skAllowDependencyReads:
		return &c.Workspace.AllowDependencyReads
	case skGitWrites:
		return &c.Git.AllowWrites
	case skGitDestructive:
		return &c.Git.AllowDestructive
	case skGitPush:
		return &c.Git.AllowPush
	case skAutoAttach:
		return &c.Workspace.AutoAttach
	case skSemEnabled:
		return &c.Semantics.Enabled
	default:
		return nil
	}
}

// durField returns the duration config field a cycle row edits and its presets.
func durField(c *config.Config, key settingKey) (*config.Duration, []string) {
	switch key {
	case skCacheTTL:
		return &c.Cache.TTL, cacheTTLOptions
	case skLSPTimeout:
		return &c.LSPQuery.Timeout, lspTimeoutOptions
	case skSemTimeout:
		return &c.Semantics.Timeout, lspTimeoutOptions
	default:
		return nil, nil
	}
}

func (m Model) setDuration(key settingKey, dir int) Model {
	ptr, presets := durField(&m.settingsCfg, key)
	if ptr == nil {
		return m
	}
	next := cycleOption(presets, durValue(*ptr, presets), dir)
	d, err := time.ParseDuration(next)
	if err != nil {
		return m
	}
	if m.persist(func(c *config.Config) { p, _ := durField(c, key); p.Duration = d }) {
		ptr.Duration = d
		m.refreshSettingsItems()
		m.settingsStatus = settingStatus(key, durLabel(key)+" → "+next)
	}
	return m
}

func durLabel(key settingKey) string {
	switch key {
	case skCacheTTL:
		return "cache ttl"
	case skLSPTimeout:
		return "lsp query timeout"
	case skSemTimeout:
		return "semantics timeout"
	default:
		return ""
	}
}

// cycleOption returns the option dir steps away from cur, wrapping around.
func cycleOption(opts []string, cur string, dir int) string {
	idx := 0
	for i, o := range opts {
		if o == cur {
			idx = i
			break
		}
	}
	idx = (idx + dir) % len(opts)
	if idx < 0 {
		idx += len(opts)
	}
	return opts[idx]
}

func (m Model) setPathStyle(style string) Model {
	if m.persist(func(c *config.Config) { c.UI.PathStyle = style }) {
		m.settingsCfg.UI.PathStyle = style
		m.refreshSettingsItems()
		m.settingsStatus = settingStatus(skPathStyle, "path style → "+style)
	}
	return m
}

// settingStatus formats the transient status line for a changed setting,
// reflecting when the change takes effect. Driven by reloadTierFor so the
// wording always matches the row's reload-tier marker.
func settingStatus(key settingKey, change string) string {
	switch reloadTierFor(key) {
	case reloadNextSession:
		return change + " · applies to new sessions"
	case reloadRestart:
		return change + " · applies on next daemon start"
	default:
		return change + " · applied live"
	}
}

func toggleLabel(key settingKey) string {
	switch key {
	case skStrict:
		return "strict edits"
	case skShowWriteDiff:
		return "show write diff"
	case skTopology:
		return "topology"
	case skTopoResyncOnAttach:
		return "resync on attach"
	case skTopoWatch:
		return "watch files"
	case skQuality:
		return "quality analysis"
	case skRefuseHomeRoots:
		return "refuse home roots"
	case skAutoAttachPersist:
		return "auto_attach_persist"
	case skAllowDependencyReads:
		return "allow_dependency_reads"
	case skGitWrites:
		return "git allow writes"
	case skGitDestructive:
		return "git allow destructive"
	case skGitPush:
		return "git allow push"
	case skAutoAttach:
		return "workspace auto-attach"
	case skSemEnabled:
		return "semantics"
	default:
		return ""
	}
}
