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
		Strict:                     false,
		RateLimitPerMinute:         120,
		PostWriteDiagnosticsMs:     300,
		ConcurrentWriteSkewMs:      100,
		ShowWriteDiff:              true,
		BlockDirtyWrites:           true,
		Fsync:                      true,
		PostWriteCrossFile:         true,
		PostWriteCrossFileSettleMs: 200,
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
		CommitTrailer:     false,
		// Ten minutes, not the two this used to be: the bound has to cover a
		// pre-commit hook that queues behind another agent's golangci-lint on the
		// shared cache, which is the normal case on a multi-agent machine and
		// routinely runs past two minutes. It is still a bound — a child that is
		// genuinely wedged cannot hold the repository lock forever.
		WriteTimeout: Duration{10 * time.Minute},
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
		ExtractTimeoutSeconds: 10,
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
	Collab: CollabConfig{
		PeerAwareness:    true,
		HintBudgetBytes:  512,
		Intents:          false,
		Mailbox:          true,
		CrossProject:     false,
		MaxExchanges:     10,
		ChatBudgetBytes:  2048,
		MaxWaitSeconds:   55,
		IntentTTLMinutes: 120,
		KnowledgeHandoff: false,
	},
	Rastro: RastroConfig{
		Enabled: false,
		Path:    "rastro",
	},
	Xcode: XcodeConfig{
		AutoBuildServer: false,
		Timeout:         Duration{2 * time.Minute},
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
			// JetBrains' Kotlin/kotlin-lsp. The `kotlin-lsp` name is the
			// PATH-portable one (Homebrew puts it there); it is a shim that
			// exec's `bin/intellij-server` with the same arguments, and prints a
			// deprecation warning to stderr on every start. The real launcher
			// lives at a version-pinned Caskroom path, so it cannot be a default
			// — point `[lsp.kotlin] command` at it directly if you prefer.
			//
			// --stdio is not optional: the server defaults to a TCP socket on
			// 127.0.0.1:9999 and ignores unknown flags SILENTLY, so getting this
			// wrong presents as a hang rather than an error. The per-root
			// --system-path cache dir cannot live here; it is appended by
			// argsFor, like jdtls's -data.
			//
			// Root markers stay Kotlin-DSL-only. Widening them to pom.xml /
			// build.gradle would contest every Java project's root for the sake
			// of the rarer Kotlin-Maven one, and a .kt file already reaches this
			// adapter by extension whatever language owns the root.
			Command:     "kotlin-lsp",
			Args:        []string{"--stdio"},
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
	// No Commands. A shipped [[command]] allow-list was tried and reverted; the
	// header comment above TargetToken in config_commands.go records why, and what
	// confinement work has to land before defaults can ship.
	Tools: ToolsConfig{Profile: "auto"},
	// No [commands] policy either: require_sandbox is off by default, and the
	// table-level deny_network default that used to live here belonged to the
	// retired shell tier. run_command entries set their own per-[[command]]
	// deny_network (default false).
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
	// Not optional: LoadProjectWithPolicy unmarshals a project's .plumb/config.toml
	// into cloneConfig(base), and go-toml MERGES a `[git.env]` sub-table or a
	// `git.env.X` dotted key into whatever map is already there. Sharing base's map
	// would let an untrusted project write straight into the daemon's live config,
	// behind the trust gate — see TestLoadProject_GitEnvCannotPoisonBase.
	out.Git.Env = maps.Clone(cfg.Git.Env)
	if cfg.LSP != nil {
		out.LSP = make(map[string]LSPConfig, len(cfg.LSP))
		for name, lspCfg := range cfg.LSP {
			out.LSP[name] = cloneLSPConfig(lspCfg)
		}
	}
	out.Tasks = cloneTasks(cfg.Tasks)
	out.Commands = cloneCommands(cfg.Commands)
	// maps.Clone preserves nil vs empty-non-nil so cloneConfig(defaults) stays
	// reflect.DeepEqual to defaults (see the slice note above).
	out.Tools.ClientProfiles = maps.Clone(cfg.Tools.ClientProfiles)
	out.UI.Keys = maps.Clone(cfg.UI.Keys)
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
	out.InitializationOptions = maps.Clone(cfg.InitializationOptions)
	return out
}
