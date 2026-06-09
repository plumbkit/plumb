package config

import (
	"maps"
	"time"
)

var defaults = Config{
	LogLevel:  "info",
	LogFormat: "text",
	UI:        UIConfig{Theme: "nordico", PathStyle: "compact"},
	Cache: CacheConfig{
		TTL:     Duration{5 * time.Minute},
		MaxSize: 1000,
	},
	Edits: EditsConfig{
		Strict:                 false,
		RateLimitPerMinute:     120,
		PostWriteDiagnosticsMs: 300,
		ConcurrentWriteSkewMs:  100,
		ShowWriteDiff:          true,
	},
	Walk: WalkConfig{
		RefuseHomeRoots: true,
	},
	Workspace: WorkspaceConfig{
		AllowDependencyReads: true,
	},
	Git: GitConfig{
		AllowWrites:       true,
		AllowDestructive:  false,
		AllowPush:         false,
		ProtectedBranches: []string{"main", "master"},
	},
	Quality: QualityConfig{
		Enabled:            false,
		Mode:               "background",
		Analysers:          []string{"golangci-lint"},
		TimeoutMs:          2000,
		MaxFindingsPerFile: 5,
	},
	Topology: TopologyConfig{
		Enabled:               true,
		MaxFileSizeBytes:      512 * 1024,
		ResyncBatch:           100,
		ResyncPauseMs:         25,
		ResyncIntervalMinutes: 60,
		Watch:                 true,
	},
	Session: SessionConfig{
		IdleThresholdMinutes: 30,
		EvictionTTLMinutes:   60,
	},
	Memory: MemoryConfig{
		Enabled:             true,
		GeneratedSummaries:  true,
		InjectHints:         true,
		HintBudgetBytes:     512,
		EpisodicBudgetBytes: 1024,
		MaxHints:            3,
		IdleSummaryMinutes:  0,
	},
	LSPQuery: LSPQueryConfig{
		Timeout: Duration{30 * time.Second},
	},
	Semantics: SemanticsConfig{
		Enabled:          false,
		Provider:         "openai",
		RerankCandidates: 50,
		Timeout:          Duration{10 * time.Second},
	},
	LSP: map[string]LSPConfig{
		"go": {
			Command:     "gopls",
			Args:        []string{},
			RootMarkers: []string{"go.mod"},
			Enabled:     true,
		},
		"python": {
			Command:     "pyright-langserver",
			Args:        []string{"--stdio"},
			RootMarkers: []string{"pyproject.toml", "setup.py", "pyrightconfig.json"},
			Enabled:     false,
		},
		"java": {
			Command:     "jdtls",
			Args:        []string{},
			RootMarkers: []string{"pom.xml", "build.gradle", "build.gradle.kts", ".classpath"},
			Enabled:     false,
			// jdtls is heavyweight (15–40 s cold start, ~0.8–1.5 GB RSS): hibernate
			// an idle JVM after 20 m and cap concurrent JVMs at 2.
			IdleTimeout:   Duration{20 * time.Minute},
			MaxWorkspaces: 2,
		},
		"rust": {
			Command:     "rust-analyzer",
			Args:        []string{},
			RootMarkers: []string{"Cargo.toml"},
			Enabled:     false,
		},
		"swift": {
			Command:     "sourcekit-lsp",
			Args:        []string{},
			RootMarkers: []string{"Package.swift"},
			Enabled:     false,
		},
		"zig": {
			Command:     "zls",
			Args:        []string{},
			RootMarkers: []string{"build.zig", "build.zig.zon"},
			Enabled:     false,
		},
		"typescript": {
			Command:     "typescript-language-server",
			Args:        []string{"--stdio"},
			RootMarkers: []string{"tsconfig.json", "jsconfig.json", "package.json"},
			Enabled:     false,
		},
		"kotlin": {
			Command:     "kotlin-language-server",
			Args:        []string{},
			RootMarkers: []string{"settings.gradle.kts", "build.gradle.kts"},
			Enabled:     false,
		},
		"html": {
			Command:     "vscode-html-language-server",
			Args:        []string{"--stdio"},
			RootMarkers: []string{"index.html"},
			Enabled:     false,
		},
	},
}

// Defaults returns a copy of the compiled-in defaults. Useful for CLI tools
// that want to compare what's in the resolved config against the baseline.
func Defaults() Config {
	return cloneConfig(defaults)
}

// cloneConfig deep-copies the maps and slices in cfg so a returned or merged
// Config never shares mutable backing storage with another load. Without this
// two loads could alias the same LSP map / default slices.
func cloneConfig(cfg Config) Config {
	out := cfg
	if cfg.Topology.ExcludePatterns != nil {
		out.Topology.ExcludePatterns = append([]string(nil), cfg.Topology.ExcludePatterns...)
	}
	if cfg.Quality.Analysers != nil {
		out.Quality.Analysers = append([]string(nil), cfg.Quality.Analysers...)
	}
	if cfg.Workspace.ExtraRoots != nil {
		out.Workspace.ExtraRoots = append([]string(nil), cfg.Workspace.ExtraRoots...)
	}
	if cfg.Workspace.ReadRoots != nil {
		out.Workspace.ReadRoots = append([]string(nil), cfg.Workspace.ReadRoots...)
	}
	if cfg.Git.ProtectedBranches != nil {
		out.Git.ProtectedBranches = append([]string(nil), cfg.Git.ProtectedBranches...)
	}
	if cfg.LSP != nil {
		out.LSP = make(map[string]LSPConfig, len(cfg.LSP))
		for name, lspCfg := range cfg.LSP {
			out.LSP[name] = cloneLSPConfig(lspCfg)
		}
	}
	return out
}

func cloneLSPConfig(cfg LSPConfig) LSPConfig {
	out := cfg
	if cfg.Args != nil {
		out.Args = append([]string(nil), cfg.Args...)
	}
	if cfg.RootMarkers != nil {
		out.RootMarkers = append([]string(nil), cfg.RootMarkers...)
	}
	if cfg.Env != nil {
		out.Env = make(map[string]string, len(cfg.Env))
		maps.Copy(out.Env, cfg.Env)
	}
	return out
}
