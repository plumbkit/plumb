package config

import (
	"maps"
	"slices"
	"time"
)

var defaults = Config{
	LogLevel:  "info",
	LogFormat: "text",
	UI:        UIConfig{Theme: "plumb", PathStyle: "compact"},
	Web:       WebConfig{Port: 8870},
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
		ChildScanDepth:       2,
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
		IdleThresholdMinutes:   30,
		EvictionTTLMinutes:     60,
		PersistState:           true,
		PersistStateTTLMinutes: 1440,
	},
	Memory: MemoryConfig{
		Enabled:             true,
		GeneratedSummaries:  true,
		InjectHints:         true,
		HintBudgetBytes:     512,
		EpisodicBudgetBytes: 1024,
		MaxHints:            3,
		IdleSummaryMinutes:  0,
		GeneratedMemoryKeep: 50,
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
			Command: "gopls",
			Args:    []string{},
			// go.work is a strong Go root too: it mounts a multi-module workspace
			// (e.g. a vendored repo or a submodule) whose modules may live in
			// subdirectories, so the go.work directory — not the nested go.mod — is
			// the root gopls wants.
			RootMarkers: []string{"go.mod", "go.work"},
			Enabled:     true,
		},
		"python": {
			Command:     "pyright-langserver",
			Args:        []string{"--stdio"},
			RootMarkers: []string{"pyproject.toml", "setup.py", "pyrightconfig.json"},
			Enabled:     true,
		},
		"java": {
			Command:     "jdtls",
			Args:        []string{},
			RootMarkers: []string{"pom.xml", "build.gradle", "build.gradle.kts", ".classpath"},
			Enabled:     true,
			// jdtls is heavyweight (15–40 s cold start, ~0.8–1.5 GB RSS): hibernate
			// an idle JVM after 20 m and cap concurrent JVMs at 2.
			IdleTimeout:   Duration{20 * time.Minute},
			MaxWorkspaces: 2,
		},
		"rust": {
			Command:     "rust-analyzer",
			Args:        []string{},
			RootMarkers: []string{"Cargo.toml"},
			Enabled:     true,
		},
		"swift": {
			Command: "sourcekit-lsp",
			Args:    []string{},
			// Package.swift is the SwiftPM root; *.xcodeproj/*.xcworkspace cover
			// Xcode-app projects that have no SwiftPM manifest (glob-matched).
			RootMarkers: []string{"Package.swift", "*.xcodeproj", "*.xcworkspace"},
			Enabled:     true,
		},
		"zig": {
			Command:     "zls",
			Args:        []string{},
			RootMarkers: []string{"build.zig", "build.zig.zon"},
			Enabled:     true,
		},
		"typescript": {
			Command:         "typescript-language-server",
			Args:            []string{"--stdio"},
			RootMarkers:     []string{"tsconfig.json", "jsconfig.json"},
			WeakRootMarkers: []string{"package.json"},
			Enabled:         true,
		},
		"kotlin": {
			Command:     "kotlin-language-server",
			Args:        []string{},
			RootMarkers: []string{"settings.gradle.kts", "build.gradle.kts"},
			Enabled:     true,
		},
		"html": {
			Command:         "vscode-html-language-server",
			Args:            []string{"--stdio"},
			WeakRootMarkers: []string{"index.html"},
			Enabled:         true,
		},
	},
	Tasks: defaultTasks(),
	Tools: ToolsConfig{Profile: "auto"},
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
	// slices.Clone preserves both nil and empty-but-non-nil slices, where
	// append([]T(nil), empty...) would collapse an empty slice to nil — that
	// asymmetry made cloneConfig(defaults) != defaults and latched
	// RestartNeeded on every fresh daemon (the defaults use Args: []string{}).
	out.Topology.ExcludePatterns = slices.Clone(cfg.Topology.ExcludePatterns)
	out.Quality.Analysers = slices.Clone(cfg.Quality.Analysers)
	out.Workspace.ExtraRoots = slices.Clone(cfg.Workspace.ExtraRoots)
	out.Workspace.ReadRoots = slices.Clone(cfg.Workspace.ReadRoots)
	out.Git.ProtectedBranches = slices.Clone(cfg.Git.ProtectedBranches)
	if cfg.LSP != nil {
		out.LSP = make(map[string]LSPConfig, len(cfg.LSP))
		for name, lspCfg := range cfg.LSP {
			out.LSP[name] = cloneLSPConfig(lspCfg)
		}
	}
	out.Tasks = cloneTasks(cfg.Tasks)
	// maps.Clone preserves nil vs empty-non-nil so cloneConfig(defaults) stays
	// reflect.DeepEqual to defaults (see the slice note above).
	out.Tools.ClientProfiles = maps.Clone(cfg.Tools.ClientProfiles)
	return out
}

func cloneLSPConfig(cfg LSPConfig) LSPConfig {
	out := cfg
	// slices.Clone / maps.Clone preserve nil vs empty-non-nil, so a cloned
	// config stays reflect.DeepEqual to its source (see cloneConfig).
	out.Args = slices.Clone(cfg.Args)
	out.RootMarkers = slices.Clone(cfg.RootMarkers)
	out.WeakRootMarkers = slices.Clone(cfg.WeakRootMarkers)
	out.Env = maps.Clone(cfg.Env)
	return out
}
