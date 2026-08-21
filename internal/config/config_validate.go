package config

import (
	"errors"
	"fmt"
	"strings"
)

func validate(cfg Config) error {
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug, info, warn, error; got %q", cfg.LogLevel)
	}
	switch cfg.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("log_format must be one of text, json; got %q", cfg.LogFormat)
	}
	if cfg.Cache.MaxSize < 0 {
		return errors.New("cache.max_size must be non-negative")
	}
	if cfg.Edits.RateLimitPerMinute < 0 {
		return errors.New("edits.rate_limit_per_minute must be non-negative (0 disables)")
	}
	if cfg.Edits.PostWriteDiagnosticsMs < 0 {
		return errors.New("edits.post_write_diagnostics_ms must be non-negative (0 disables)")
	}
	if cfg.Edits.ConcurrentWriteSkewMs < 0 {
		return errors.New("edits.concurrent_write_skew_ms must be non-negative")
	}
	if cfg.Edits.PostWriteCrossFileSettleMs < 0 {
		return errors.New("edits.post_write_cross_file_settle_ms must be non-negative (0 disables the grace)")
	}
	if cfg.LSPQuery.Timeout.Duration < 0 {
		return errors.New("lsp_query.timeout must be non-negative (0 disables)")
	}
	if cfg.Xcode.AutoBuildServer && cfg.Xcode.Timeout.Duration <= 0 {
		return errors.New("xcode.timeout must be positive")
	}
	for _, check := range []func() error{
		func() error { return validateGit(cfg.Git) },
		func() error { return validateQuality(cfg.Quality) },
		func() error { return validateTopology(cfg.Topology) },
		func() error { return validateSemantics(cfg.Semantics) },
		func() error { return validateMemory(cfg.Memory) },
		func() error { return validateCollab(cfg.Collab) },
		func() error { return validateTasks(cfg.Tasks) },
		func() error { return validateCommands(cfg.Commands) },
		func() error { return validateTools(cfg.Tools) },
		func() error { return validateLSP(cfg.LSP) },
	} {
		if err := check(); err != nil {
			return err
		}
	}
	switch cfg.UI.PathStyle {
	case "", "compact", "truncate-middle", "full":
	default:
		return fmt.Errorf("ui.path_style must be compact, truncate-middle, or full; got %q", cfg.UI.PathStyle)
	}
	return nil
}

// validateLSP checks the per-language [lsp.<lang>] tables: an enabled server
// needs a command, and the diagnostics knob must be one of the accepted enum
// values (empty resolves to auto).
func validateLSP(lsp map[string]LSPConfig) error {
	for name, l := range lsp {
		if l.Enabled && l.Command == "" {
			return fmt.Errorf("lsp.%s.command must be set when enabled", name)
		}
		switch l.Diagnostics {
		case "", "auto", "push", "pull":
		default:
			return fmt.Errorf("lsp.%s.diagnostics must be auto, push, or pull; got %q", name, l.Diagnostics)
		}
	}
	return nil
}

// validateGit rejects a [git] env entry whose NAME could not survive the trip
// into a `KEY=VALUE` environment slice. An empty name, or one containing '='
// or a NUL, does not produce the variable it appears to name: it either
// corrupts the entry or splits it, so the value silently lands on a different
// variable than the one written in the config. Reject it at load rather than
// hand the child a malformed environment. Values are unrestricted — an
// environment value may legitimately contain anything but NUL.
func validateGit(g GitConfig) error {
	// Negative is rejected; zero is not. Zero resolves to the compiled default at
	// the point of use, which keeps a zero-value GitConfig (every test that builds
	// one, and any consumer constructing a policy by hand) meaning "the default
	// bound" rather than "no bound at all" — the one value that must never be
	// reachable, since an unbounded child can hold the repository lock forever.
	if g.WriteTimeout.Duration < 0 {
		return fmt.Errorf("git.write_timeout must not be negative (got %s)", g.WriteTimeout.Duration)
	}
	for k := range g.Env {
		switch {
		case k == "":
			return errors.New("git.env: an environment variable name must not be empty")
		case strings.ContainsAny(k, "=\x00"):
			return fmt.Errorf("git.env: environment variable name %q must not contain '=' or a NUL byte", k)
		}
	}
	for k, v := range g.Env {
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("git.env.%s: an environment variable value must not contain a NUL byte", k)
		}
	}
	return nil
}

func validateQuality(q QualityConfig) error {
	switch q.Mode {
	case "", "background", "sync":
	default:
		return fmt.Errorf("quality.mode must be \"background\" or \"sync\"; got %q", q.Mode)
	}
	if q.TimeoutMs < 0 {
		return errors.New("quality.timeout_ms must be non-negative")
	}
	if q.MaxFindingsPerFile < 0 {
		return errors.New("quality.max_findings_per_file must be non-negative")
	}
	return nil
}

func validateSemantics(s SemanticsConfig) error {
	switch s.Provider {
	case "", "openai", "voyage", "jina", "mistral", "cohere", "custom":
	default:
		return fmt.Errorf("semantics.provider must be one of openai, voyage, jina, mistral, cohere, custom; got %q", s.Provider)
	}
	if s.Enabled && s.Provider == "custom" && s.BaseURL == "" {
		return errors.New("semantics.base_url is required when provider = custom and enabled = true")
	}
	if s.RerankCandidates < 0 {
		return errors.New("semantics.rerank_candidates must be non-negative (0 uses the default)")
	}
	if s.Timeout.Duration < 0 {
		return errors.New("semantics.timeout must be non-negative")
	}
	return nil
}

func validateMemory(m MemoryConfig) error {
	if m.HintBudgetBytes < 0 {
		return errors.New("memory.hint_budget_bytes must be non-negative")
	}
	if m.EpisodicBudgetBytes < 0 {
		return errors.New("memory.episodic_budget_bytes must be non-negative")
	}
	if m.MaxHints < 0 {
		return errors.New("memory.max_hints must be non-negative")
	}
	if m.IdleSummaryMinutes < 0 {
		return errors.New("memory.idle_summary_minutes must be non-negative")
	}
	if m.GeneratedMemoryKeep < 0 {
		return errors.New("memory.generated_memory_keep must be non-negative (0 disables pruning)")
	}
	return nil
}

// maxCollabWaitSeconds bounds [collab] max_wait_seconds. A blocking
// check_messages holds a request goroutine for its whole duration and keeps the
// session's last-seen timestamp fresh, so an unbounded value would both outlive
// the client's own MCP call timeout and make the session immortal to the idle
// reaper. Five minutes is far beyond any real client timeout.
const maxCollabWaitSeconds = 300

func validateCollab(c CollabConfig) error {
	if c.HintBudgetBytes < 0 {
		return errors.New("collab.hint_budget_bytes must be non-negative")
	}
	if c.IntentTTLMinutes < 0 {
		return errors.New("collab.intent_ttl_minutes must be non-negative (0 uses the default)")
	}
	if c.MaxExchanges < 0 {
		return errors.New("collab.max_exchanges must be non-negative (0 uses the default)")
	}
	if c.ChatBudgetBytes < 0 {
		return errors.New("collab.chat_budget_bytes must be non-negative (0 uses the default)")
	}
	if c.MaxWaitSeconds < 0 {
		return errors.New("collab.max_wait_seconds must be non-negative (0 uses the default)")
	}
	// An unbounded value would park a request goroutine for as long as it says,
	// well past any client's own call timeout, and keep the session looking
	// active so the idle reaper never evicts it.
	if c.MaxWaitSeconds > maxCollabWaitSeconds {
		return fmt.Errorf("collab.max_wait_seconds must be at most %d (a longer wait outlives the client's own call timeout)", maxCollabWaitSeconds)
	}
	return nil
}

// validateTasks rejects a task command that would not run as a bare argv (a
// shell metacharacter). An empty slot is always valid. Every slot — including
// verify — is checked: although verify is a composite that the runner ignores,
// the field is agent-writable, so an un-validated string there would let a
// metacharacter command be staged unchecked. Reading the struct fields directly
// (not Get, which returns "" for verify) ensures verify is covered too.
func validateTasks(tasks map[string]TasksConfig) error {
	for lang, t := range tasks {
		slots := []struct{ name, cmd string }{
			{"build", t.Build}, {"lint", t.Lint}, {"test", t.Test}, {"e2e", t.E2E}, {"verify", t.Verify},
		}
		for _, sl := range slots {
			if _, err := ParseTaskCommand(sl.cmd); err != nil {
				return fmt.Errorf("tasks.%s.%s: %w", lang, sl.name, err)
			}
		}
		if err := validateExtraTaskSlots(lang, t.Extra); err != nil {
			return err
		}
		// Same rule as a [[command]] working_dir: relative to the workspace root and
		// no ".." escape. This is the LEXICAL half; the resolution-time half runs
		// when the directory is made absolute, because a relative path naming a
		// symlink passes every lexical check and still lands outside the tree.
		if err := validateCommandWorkingDir(t.WorkingDir); err != nil {
			return fmt.Errorf("tasks.%s: %w", lang, err)
		}
	}
	return nil
}

// validateExtraTaskSlots applies to a project-named slot every rule a built-in
// gets, and two it needs on top.
//
// The command check is the one that matters. The comment on validateTasks
// states the stake: an un-validated command string lets a shell metacharacter
// reach a slot that the runner will exec. Extras are agent-reachable through
// run_task exactly like the built-ins, so leaving them out here would open
// precisely the hole the exhaustive built-in loop exists to close.
//
// The name rules are the two a built-in cannot need. A malformed name is
// rejected rather than ignored, because an ignored slot is a command the user
// wrote and believes is configured. A name colliding with a built-in is
// rejected rather than shadowing it: Get answers from the struct field, so the
// extra would be silently dead — and `verify` would be worse than dead, since
// it is a composite the runner synthesises and never reads a command for.
func validateExtraTaskSlots(lang string, extra map[string]string) error {
	for name, cmd := range extra {
		if !ValidTaskSlotName(name) {
			return fmt.Errorf("tasks.%s.%s: %q is not a valid slot name "+
				"(lowercase letter first, then letters, digits, _ or -, max 32 characters)", lang, name, name)
		}
		if IsBuiltinTaskSlot(name) {
			return fmt.Errorf("tasks.%s.%s: %q is a built-in slot and cannot be redefined as an extra", lang, name, name)
		}
		if _, err := ParseTaskCommand(cmd); err != nil {
			return fmt.Errorf("tasks.%s.%s: %w", lang, name, err)
		}
	}
	return nil
}

// validateTools rejects an unknown tool profile, in both the global Profile and
// any per-client override. An empty value is allowed so an absent per-client
// entry falls through to Profile.
func validateTools(t ToolsConfig) error {
	if err := validateToolProfile("tools.profile", t.Profile); err != nil {
		return err
	}
	for client, p := range t.ClientProfiles {
		if err := validateToolProfile("tools.client_profiles."+client, p); err != nil {
			return err
		}
	}
	return nil
}

func validateToolProfile(key, v string) error {
	switch v {
	case "", "auto", "lean", "full":
		return nil
	default:
		return fmt.Errorf("%s must be one of auto, lean, full; got %q", key, v)
	}
}

func validateTopology(tp TopologyConfig) error {
	if tp.MaxFileSizeBytes < 0 {
		return errors.New("topology.max_file_size_bytes must be non-negative (0 uses the default)")
	}
	if tp.ExtractTimeoutSeconds < 0 {
		return errors.New("topology.extract_timeout_seconds must be non-negative (0 means the built-in 2-minute ceiling)")
	}
	if tp.ResyncBatch < 0 {
		return errors.New("topology.resync_batch must be non-negative (0 disables pacing)")
	}
	if tp.ResyncPauseMs < 0 {
		return errors.New("topology.resync_pause_ms must be non-negative (0 disables pacing)")
	}
	if tp.ResyncIntervalMinutes < 0 {
		return errors.New("topology.resync_interval_minutes must be non-negative (0 disables periodic resync)")
	}
	return nil
}
