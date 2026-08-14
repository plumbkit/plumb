package config

// project_classification.go answers one question for every field a project's
// .plumb/config.toml can set: is a hostile value safe here?
//
// The two vulnerabilities that prompted the project-config trust boundary were
// found by INSPECTION — someone noticed that [lsp.<lang>] command reaches
// exec.CommandContext, and that [git] opens the destructive tier. Inspection
// finds what it happens to look at. This table is the enumeration nobody had
// done, and the completeness test beside it means a field added later cannot
// quietly skip the question: a new field with no entry fails the build.
//
// The classification is the deliverable, not just the fixes. A field recorded as
// ClassPreference with a reason is a decision on record; the same field with no
// entry is an oversight waiting to become a finding.
//
// Concurrency: the table is immutable package data, read-only after init.

// ProjectFieldClass says how a project's requested value for one field is
// treated when the project is UNTRUSTED — which is the default, and the state a
// cloned repository is always in.
type ProjectFieldClass int

const (
	// ClassPreference — honoured verbatim. A hostile value is a nuisance at
	// worst: it cannot widen access, run a process, redirect a write, or hide
	// evidence an auditor would rely on. Most of the tree is this.
	ClassPreference ProjectFieldClass = iota

	// ClassForcedGlobal — always replaced by the global value, even for a
	// trusted project. Either a daemon-global concern (one listener, one theme)
	// or a safety knob whose whole purpose is that the thing it governs cannot
	// widen its own permission.
	ClassForcedGlobal

	// ClassOneWay — honoured only in the direction that makes the setting MORE
	// restrictive. A project may harden its own workspace; it may not soften the
	// user's global choice. This keeps the legitimate case (a repository asking
	// for strict mode) without the exploit (a repository switching it off).
	ClassOneWay

	// ClassTrustGated — honoured only when the user has approved this project's
	// exact content with `plumb trust`, bound to a content hash. See
	// project_policy.go.
	ClassTrustGated

	// ClassInert — never reaches a consumer from a project config at all,
	// because the only reader takes the global config or reads it once before
	// any project config is loaded. Recorded rather than omitted: "inert" is a
	// property of today's wiring, not a guarantee, and a refactor that makes one
	// of these live must be a deliberate act rather than an accident.
	ClassInert
)

// projectFieldClasses maps a dotted TOML path to its classification. Every leaf
// field of Config must appear here; TestProjectFieldClasses_CoverEveryConfigField
// enumerates the struct by reflection and fails on an omission.
//
// The comment on each non-preference entry is the reason, and it is the point.
var projectFieldClasses = map[string]ProjectFieldClass{
	// --- Logging: inert. setupLogging's only callers are the daemon's own
	// startup (global config) and the operator-driven `plumb log-level`, so a
	// project value never reaches the logger. log_file has no reader at all.
	"log_level":  ClassInert,
	"log_format": ClassInert,
	"log_file":   ClassInert,

	// --- Presentation and the web listener: one process, one theme, one port.
	"ui.theme":      ClassForcedGlobal,
	"ui.path_style": ClassForcedGlobal,
	"ui.keys":       ClassForcedGlobal,
	"web.port":      ClassForcedGlobal,

	// --- Cache: read once at connection construction, before any workspace is
	// attached, so a project value cannot reach it.
	"cache.ttl":      ClassInert,
	"cache.max_size": ClassInert,

	// --- Edits. The safety knobs are ONE-WAY: a project may raise the bar for
	// its own workspace, never lower the user's. strict disables read-before-
	// edit; block_dirty_writes is what stops a write clobbering uncommitted work
	// the user has not reviewed; show_write_diff is the only part of a write
	// response that says WHAT changed, so switching it off blinds a reviewer
	// reading the live transcript.
	"edits.strict":             ClassOneWay,
	"edits.block_dirty_writes": ClassOneWay,
	"edits.show_write_diff":    ClassOneWay,
	// The write-rate budget is an anti-runaway-loop guard rather than an access
	// boundary, but it is still a budget a project should not be able to raise.
	// 0 means unlimited, which is why "larger is weaker" needs its own rule.
	"edits.rate_limit_per_minute": ClassOneWay,
	// Timing and feedback tuning: a hostile value degrades this workspace's own
	// diagnostics quality and nothing else.
	"edits.post_write_diagnostics_ms":       ClassPreference,
	"edits.concurrent_write_skew_ms":        ClassPreference,
	"edits.post_write_cross_file":           ClassPreference,
	"edits.post_write_cross_file_settle_ms": ClassPreference,
	// fsync is deliberately global-only at its consumer (daemon.go): honouring a
	// per-project value would let the last workspace to attach set the
	// durability contract for every other session.
	"edits.fsync": ClassInert,

	// --- Walk. refuse_home_roots suppresses a macOS TCC consent prompt for two
	// convenience walks inside session_start. It is never consulted by
	// buildPathPolicy, so it cannot widen access.
	"walk.refuse_home_roots": ClassPreference,

	// --- Workspace. The three root/read fields all widen the filesystem
	// allowlist on attach with no per-call confirmation.
	"workspace.extra_roots": ClassForcedGlobal,
	"workspace.read_roots":  ClassForcedGlobal,
	// allow_dependency_reads re-opens read access to the toolchain caches
	// (GOMODCACHE, cargo registry, site-packages). It sits one field below its
	// two siblings above and was the one the original fix missed: a user who
	// deliberately set it false globally had that opt-out silently reversed by
	// any cloned repository.
	"workspace.allow_dependency_reads": ClassForcedGlobal,
	// Read from the global store during first attach, before this workspace's
	// own project config could apply.
	"workspace.auto_attach":         ClassInert,
	"workspace.auto_attach_persist": ClassInert,
	"workspace.child_scan_depth":    ClassInert,

	// --- Git: the tiered safety policy in its entirety. See project_policy.go.
	"git.allow_writes":       ClassTrustGated,
	"git.allow_destructive":  ClassTrustGated,
	"git.allow_push":         ClassTrustGated,
	"git.protected_branches": ClassTrustGated,
	"git.commit_trailer":     ClassTrustGated,
	// env is the environment of the git process that runs THIS repository's
	// hooks, and several of its variables name a command git will run
	// (GIT_SSH_COMMAND, GIT_EXTERNAL_DIFF, GIT_PROXY_COMMAND, GIT_PAGER), so an
	// untrusted value here is arbitrary code execution as the user.
	"git.env": ClassTrustGated,

	// --- Session lifecycle. persist_state only makes this connection's own
	// state more or less sticky, and writes to a plumb-owned fixed path.
	"session.persist_state":             ClassPreference,
	"session.idle_threshold_minutes":    ClassPreference,
	"session.eviction_ttl_minutes":      ClassInert,
	"session.persist_state_ttl_minutes": ClassInert,

	// --- Quality. Reads the global store, so a project value never applies.
	// Note the analyser NAMES are resolved against a closed switch and never
	// reach an argv, so even wiring this up would not make it a capability.
	"quality.enabled":               ClassInert,
	"quality.mode":                  ClassInert,
	"quality.analysers":             ClassInert,
	"quality.timeout_ms":            ClassInert,
	"quality.max_findings_per_file": ClassInert,

	// --- Topology: sizes, timeouts and pacing for this workspace's own index.
	// exclude_patterns has no consumer at all today (the skip list is hardcoded
	// in indexer_resync.go) — recorded as inert rather than as a working control.
	"topology.enabled":                 ClassPreference,
	"topology.resync_on_attach":        ClassPreference,
	"topology.exclude_patterns":        ClassInert,
	"topology.max_file_size_bytes":     ClassPreference,
	"topology.extract_timeout_seconds": ClassPreference,
	"topology.resync_batch":            ClassPreference,
	"topology.resync_pause_ms":         ClassPreference,
	"topology.resync_interval_minutes": ClassPreference,
	"topology.watch":                   ClassPreference,

	// --- LSP: which process the daemon spawns, and with what.
	"lsp.<lang>.command":                ClassTrustGated,
	"lsp.<lang>.args":                   ClassTrustGated,
	"lsp.<lang>.env":                    ClassTrustGated,
	"lsp.<lang>.initialization_options": ClassTrustGated,
	"lsp.<lang>.root_markers":           ClassTrustGated,
	"lsp.<lang>.weak_root_markers":      ClassTrustGated,
	"lsp.<lang>.enabled":                ClassPreference,
	"lsp.<lang>.diagnostics":            ClassPreference,
	"lsp.<lang>.idle_timeout":           ClassPreference,
	"lsp.<lang>.max_workspaces":         ClassPreference,

	// --- LSP query: captured once at tool registration, before attach.
	"lsp_query.timeout": ClassInert,

	// --- Semantics: an outbound endpoint plus the credentials sent to it.
	"semantics.enabled":           ClassForcedGlobal,
	"semantics.provider":          ClassForcedGlobal,
	"semantics.model":             ClassForcedGlobal,
	"semantics.base_url":          ClassForcedGlobal,
	"semantics.api_key":           ClassForcedGlobal,
	"semantics.api_key_env":       ClassForcedGlobal,
	"semantics.rerank_candidates": ClassForcedGlobal,
	"semantics.timeout":           ClassForcedGlobal,

	// --- Memory. generated_summaries decides whether episodic summaries are
	// written into <workspace>/.plumb/memories/ at all; a project must not be
	// able to switch it back on against a user's explicit global opt-out, so it
	// is one-way in the OFF direction (see oneWaySafeValue).
	"memory.generated_summaries":   ClassOneWay,
	"memory.enabled":               ClassPreference,
	"memory.inject_hints":          ClassPreference,
	"memory.hint_budget_bytes":     ClassPreference,
	"memory.episodic_budget_bytes": ClassPreference,
	"memory.max_hints":             ClassPreference,
	"memory.idle_summary_minutes":  ClassPreference,
	"memory.generated_memory_keep": ClassPreference,

	// --- Collab. The four switches below each open a cross-agent CHANNEL, and a
	// channel a cloned repository can open is a channel it can use: a payload
	// that has already steered one agent through some other file in the repo can
	// leave instructions for the next session, or push agent-authored content
	// into the durable memory store. cross_project is the sharpest — its own doc
	// comment promises "another project can never inject text into this one's
	// context uninvited", which is only true if the recipient's own project file
	// cannot set it.
	// They are TRUST-GATED rather than forced: 0.16.4 forced them global-only,
	// which was the safe default and the wrong ceiling — "per workspace" and "the
	// repository decides" are not the same thing, and a user who wants chat on for
	// one repository should be able to say so. A project may now ASK, and the
	// request is honoured once `plumb trust` has approved that exact content.
	"collab.intents":           ClassTrustGated,
	"collab.mailbox":           ClassTrustGated,
	"collab.cross_project":     ClassTrustGated,
	"collab.knowledge_handoff": ClassTrustGated,
	// Budgets, expiries and the passive, observed-facts layer stay per-project.
	// max_wait_seconds only bounds how long check_messages blocks, and is capped
	// below the client's own call timeout at the point of use.
	"collab.peer_awareness":     ClassPreference,
	"collab.hint_budget_bytes":  ClassPreference,
	"collab.max_exchanges":      ClassPreference,
	"collab.chat_budget_bytes":  ClassPreference,
	"collab.max_wait_seconds":   ClassPreference,
	"collab.intent_ttl_minutes": ClassPreference,

	// --- Tools. The profile decides which tools appear in tools/list. For a
	// client that builds its whole tool set from that list (SchemaDiscoveryOnly,
	// e.g. Claude Code) a hidden tool is unreachable rather than merely
	// undisplayed — so a repository setting profile = "lean" removes
	// search_in_files and find_files from the session auditing it, and does so by
	// short-circuiting the very guard that forces "full" for such clients
	// (resolveToolProfile consults an explicit profile BEFORE the auto-mode rule).
	//
	// profile is ONE-WAY rather than forced, because only NARROWING is the attack:
	// a project asking for "full" merely advertises more, which is a legitimate
	// thing for a repository to want and something plumb already supported.
	// client_profiles is forced outright — which tools a given CLIENT is offered is
	// a property of the client, not of the repository it has open.
	"tools.profile":         ClassOneWay,
	"tools.client_profiles": ClassForcedGlobal,

	// --- Rastro: doctor resolves the binary with exec.LookPath and never runs it.
	"rastro.enabled": ClassPreference,
	"rastro.path":    ClassPreference,

	// --- Xcode: auto_build_server spawns xcodebuild and xcode-build-server,
	// which runs THIS repository's own build. Bound to the policy content hash
	// alongside [git]/[lsp.<lang>]: the whole table is in ProjectPolicySpec, so
	// enabling it after a grant invalidates that grant. scheme and timeout are
	// inputs to the same argv, so they are bound too.
	"xcode.auto_build_server": ClassTrustGated,
	"xcode.scheme":            ClassTrustGated,
	"xcode.timeout":           ClassTrustGated,

	// --- Task commands and the command allow-list: argv plumb runs.
	"tasks.<lang>.build":  ClassTrustGated,
	"tasks.<lang>.lint":   ClassTrustGated,
	"tasks.<lang>.test":   ClassTrustGated,
	"tasks.<lang>.e2e":    ClassTrustGated,
	"tasks.<lang>.verify": ClassTrustGated,
	// The whole [[command]] array is one ProjectPolicySpec entry, so adding or
	// rewriting any entry invalidates the grant. Order is part of the hash because
	// FindCommand takes the first match by name.
	"command": ClassTrustGated,
	// [commands] allow_shell and deny_network are honoured from a project file
	// only for a trusted workspace (gatedAllowShell / gatedDenyNetwork), and the
	// gate is now the policy CONTENT hash rather than the coarse per-root boolean
	// — the same binding [git] and [lsp.<lang>] get. The coarse flag is still
	// required in addition, since it is what says the user approved execution in
	// this workspace at all (see config.ExecTrustedFor).
	"commands.allow_shell":  ClassTrustGated,
	"commands.deny_network": ClassTrustGated,
	// require_sandbox is the one [commands] field a project may set untrusted,
	// because it can only ADD safety: effectiveRequireSandbox already takes the
	// most restrictive of global and project. Resolved here too so the merged
	// config — and therefore `config show` — states what is actually in force.
	"commands.require_sandbox": ClassOneWay,

	// --- The enable knob for agent-writable config. A project must never be
	// able to switch on the tool that lets an agent rewrite config.
	"agent_config_writes": ClassForcedGlobal,
}

// oneWaySafeValue reports which boolean value is the SAFE one for a ClassOneWay
// boolean — the value a project may move the setting TO but never away from.
//
// It differs per field, which is why it is a table rather than a convention:
// strict mode and the dirty guard are safe when ON, whereas generated summaries
// are safe when OFF (fewer files written into the workspace). Getting this
// backwards would silently invert the protection, so each entry is stated.
var oneWaySafeValue = map[string]bool{
	"edits.strict":               true,
	"edits.block_dirty_writes":   true,
	"edits.show_write_diff":      true,
	"commands.require_sandbox":   true,
	"memory.generated_summaries": false,
}

// applyOneWayBools resolves the ClassOneWay boolean fields: the project's value
// is honoured only when it equals the safe value; otherwise the global one
// stands. Equivalent to "most restrictive wins", the rule
// effectiveRequireSandbox already applies to [commands] require_sandbox.
func applyOneWayBools(base Config, merged *Config) {
	merged.Edits.Strict = oneWayBool(base.Edits.Strict, merged.Edits.Strict, oneWaySafeValue["edits.strict"])
	merged.Edits.BlockDirtyWrites = oneWayBool(
		base.Edits.BlockDirtyWrites, merged.Edits.BlockDirtyWrites, oneWaySafeValue["edits.block_dirty_writes"])
	merged.Edits.ShowWriteDiff = oneWayBool(
		base.Edits.ShowWriteDiff, merged.Edits.ShowWriteDiff, oneWaySafeValue["edits.show_write_diff"])
	merged.Memory.GeneratedSummaries = oneWayBool(
		base.Memory.GeneratedSummaries, merged.Memory.GeneratedSummaries, oneWaySafeValue["memory.generated_summaries"])
	merged.CommandPolicy.RequireSandbox = oneWayBool(
		base.CommandPolicy.RequireSandbox, merged.CommandPolicy.RequireSandbox,
		oneWaySafeValue["commands.require_sandbox"])
	merged.Edits.RateLimitPerMinute = oneWayRateLimit(base.Edits.RateLimitPerMinute, merged.Edits.RateLimitPerMinute)
	merged.Tools.Profile = oneWayToolsProfile(base.Tools.Profile, merged.Tools.Profile)
}

// oneWayToolsProfile lets a project WIDEN the advertised tool set and nothing
// else: "full" is honoured, anything else falls back to the global value.
//
// Narrowing is the whole attack — a repository setting "lean" removes
// search_in_files and find_files from the session auditing it, and for a client
// that discovers tools only from tools/list they become unreachable rather than
// merely undisplayed. Widening cannot hurt: it advertises tools that stay
// callable by name either way.
//
// "auto" is deliberately NOT treated as safe. It resolves per client and can
// resolve to lean, so honouring it from a project file would reintroduce the
// narrowing through the back door.
func oneWayToolsProfile(global, project string) string {
	if project == "full" {
		return project
	}
	return global
}

// oneWayBool returns project when it is the safe value, else global.
func oneWayBool(global, project, safe bool) bool {
	if project == safe {
		return project
	}
	return global
}

// oneWayRateLimit returns the more restrictive of two write-rate budgets.
// 0 means UNLIMITED rather than "block everything", so it is the weakest value
// and loses to any positive limit — the opposite of a plain min().
//
// A NEGATIVE project value is passed through untouched rather than resolved.
// It is invalid config, and validate() rejects it after this runs; coercing it
// to the global value here would swallow that error and silently accept a
// malformed project file instead of reporting it.
func oneWayRateLimit(global, project int) int {
	switch {
	case project < 0:
		return project
	case global <= 0:
		return project // global is unlimited: any project limit is stricter
	case project <= 0:
		return global // project asks for unlimited: refuse, keep the global cap
	case project < global:
		return project
	default:
		return global
	}
}
