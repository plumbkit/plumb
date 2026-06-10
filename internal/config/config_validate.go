package config

import "fmt"

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
		return fmt.Errorf("cache.max_size must be non-negative")
	}
	if cfg.Edits.RateLimitPerMinute < 0 {
		return fmt.Errorf("edits.rate_limit_per_minute must be non-negative (0 disables)")
	}
	if cfg.Edits.PostWriteDiagnosticsMs < 0 {
		return fmt.Errorf("edits.post_write_diagnostics_ms must be non-negative (0 disables)")
	}
	if cfg.Edits.ConcurrentWriteSkewMs < 0 {
		return fmt.Errorf("edits.concurrent_write_skew_ms must be non-negative")
	}
	if err := validateQuality(cfg.Quality); err != nil {
		return err
	}
	if cfg.LSPQuery.Timeout.Duration < 0 {
		return fmt.Errorf("lsp_query.timeout must be non-negative (0 disables)")
	}
	if err := validateTopology(cfg.Topology); err != nil {
		return err
	}
	if err := validateSemantics(cfg.Semantics); err != nil {
		return err
	}
	if err := validateMemory(cfg.Memory); err != nil {
		return err
	}
	switch cfg.UI.PathStyle {
	case "", "compact", "truncate-middle", "full":
	default:
		return fmt.Errorf("ui.path_style must be compact, truncate-middle, or full; got %q", cfg.UI.PathStyle)
	}
	for name, lsp := range cfg.LSP {
		if lsp.Enabled && lsp.Command == "" {
			return fmt.Errorf("lsp.%s.command must be set when enabled", name)
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
		return fmt.Errorf("quality.timeout_ms must be non-negative")
	}
	if q.MaxFindingsPerFile < 0 {
		return fmt.Errorf("quality.max_findings_per_file must be non-negative")
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
		return fmt.Errorf("semantics.base_url is required when provider = custom and enabled = true")
	}
	if s.RerankCandidates < 0 {
		return fmt.Errorf("semantics.rerank_candidates must be non-negative (0 uses the default)")
	}
	if s.Timeout.Duration < 0 {
		return fmt.Errorf("semantics.timeout must be non-negative")
	}
	return nil
}

func validateMemory(m MemoryConfig) error {
	if m.HintBudgetBytes < 0 {
		return fmt.Errorf("memory.hint_budget_bytes must be non-negative")
	}
	if m.EpisodicBudgetBytes < 0 {
		return fmt.Errorf("memory.episodic_budget_bytes must be non-negative")
	}
	if m.MaxHints < 0 {
		return fmt.Errorf("memory.max_hints must be non-negative")
	}
	if m.IdleSummaryMinutes < 0 {
		return fmt.Errorf("memory.idle_summary_minutes must be non-negative")
	}
	if m.GeneratedMemoryKeep < 0 {
		return fmt.Errorf("memory.generated_memory_keep must be non-negative (0 disables pruning)")
	}
	return nil
}

func validateTopology(tp TopologyConfig) error {
	if tp.MaxFileSizeBytes < 0 {
		return fmt.Errorf("topology.max_file_size_bytes must be non-negative (0 uses the default)")
	}
	if tp.ResyncBatch < 0 {
		return fmt.Errorf("topology.resync_batch must be non-negative (0 disables pacing)")
	}
	if tp.ResyncPauseMs < 0 {
		return fmt.Errorf("topology.resync_pause_ms must be non-negative (0 disables pacing)")
	}
	if tp.ResyncIntervalMinutes < 0 {
		return fmt.Errorf("topology.resync_interval_minutes must be non-negative (0 disables periodic resync)")
	}
	return nil
}
