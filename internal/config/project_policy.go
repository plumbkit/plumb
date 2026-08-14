package config

import (
	"maps"
	"slices"
	"sort"
	"strings"
)

// project_policy.go defines the CAPABILITY-GRANTING half of a project config —
// the settings a workspace's .plumb/config.toml can only have honoured once the
// user has trusted that exact request — and the code that forces them back to
// the trusted global config when it is not trusted.
//
// The problem this solves. A project config is an untrusted surface: cloning a
// repository ships one, and it takes effect on attach with no prompt. Two of its
// sections grant capability rather than express a preference — [lsp.<lang>]
// command/args/env/initialization_options decide WHICH process the daemon
// spawns and with what (straight through to exec.CommandContext), and [git] is
// the git tool's tiered safety policy. Honouring either unconditionally is
// arbitrary code execution and remote history destruction respectively.
//
// Forcing them global-only is the safe default but the wrong ceiling: projects
// legitimately differ, and a user who wants `[lsp.html] root_markers =
// ['index.html']` for one repository should be able to have it. So this follows
// the model plumb already uses for project-supplied task commands (trust.go):
// the user approves a project's request ONCE, out of band, with `plumb trust`,
// and the grant is bound to the exact content approved. The record lives in
// plumb's own data dir, never in the project, so a cloned repository cannot mark
// itself trusted — the VS Code workspace-trust pattern.
//
// Concurrency: ProjectPolicySpec values are immutable once built.

// PolicyEntry is one capability-granting key a project config sets: its dotted
// TOML path and the raw value the project asked for. Value is the value as
// decoded from the project's own file — NOT the value in effect, which is the
// global one whenever the request is untrusted.
type PolicyEntry struct {
	Key   string
	Value any
}

// ProjectPolicySpec is the complete set of capability-granting keys a
// workspace's project config sets, sorted by Key. It is the unit of trust: a
// trust grant binds to the hash of one exact spec, so adding, removing, or
// modifying any key invalidates it and requires a fresh `plumb trust` (this is
// what closes the TOCTOU where a repository is trusted and then rewrites its own
// `command`). An empty spec means the project asks for nothing that needs trust.
type ProjectPolicySpec []PolicyEntry

// IsEmpty reports whether the project config sets no capability-granting key at
// all — the common case, in which nothing is forced back, nothing is prompted
// for, and no trust lookup happens.
func (s ProjectPolicySpec) IsEmpty() bool { return len(s) == 0 }

// Keys returns the dotted TOML paths in the spec, in order.
func (s ProjectPolicySpec) Keys() []string {
	out := make([]string, len(s))
	for i, e := range s {
		out[i] = e.Key
	}
	return out
}

// Describe renders the spec as `key = value` lines for disclosure surfaces
// (`plumb trust`, `plumb doctor`, `plumb config show`). A user deciding whether
// to trust a project needs the VALUES, not just which keys were touched.
func (s ProjectPolicySpec) Describe() []string {
	out := make([]string, len(s))
	for i, e := range s {
		out[i] = e.Key + " = " + policyDisplay(e.Value)
	}
	return out
}

// policyLSPFreeFields are the per-language [lsp.<lang>] fields that are NOT gated
// on trust, because none of them can change which process runs or what it is fed:
// diagnostics (push/pull protocol negotiation), enabled (a project switching a
// language off, or on against the user's own trusted command), and idle_timeout /
// max_workspaces (hibernation and eviction budgets).
//
// This list is an ALLOW-list, and that direction is the whole point. The gated
// set used to be the enumerated one — command, args, env,
// initialization_options, root_markers, weak_root_markers — matched by exact
// string against the project's raw TOML. go-toml/v2 matches a key to a struct tag
// case-insensitively, so `Command`, `COMMAND` and `Root_Markers` all reached
// LSPConfig and none of them matched the enumeration: they were absent from the
// spec, from the disclosure, from every visibility surface, and from the hash. A
// repository could therefore ship `Command = "/bin/sh"`, have `plumb trust`
// disclose nothing, and be executed on attach — or, worse, be trusted for
// something innocuous and then ADD `COMMAND` later without invalidating the
// grant, which is precisely the TOCTOU this design exists to close.
//
// Enumerating what is SAFE inverts that failure mode. Any key an [lsp.<lang>]
// table holds that is not one of these four — a fold variant, a misspelling, a
// field added to LSPConfig by a later change and not thought about here — is
// gated, disclosed, and hashed. Over-gating an inert key costs a line of
// disclosure and a re-trust; under-gating one costs arbitrary code execution.
//
// [git] needs no equivalent list: it is taken whole, key by key, because every
// field in it is a safety decision. That reasoning was already written down there
// and the LSP half did not follow it. It does now.
var policyLSPFreeFields = []string{"enabled", "diagnostics", "idle_timeout", "max_workspaces"}

// isFreeLSPField reports whether an [lsp.<lang>] key is one of the provably inert
// fields. Comparison is case-insensitive to mirror go-toml/v2's own key matching,
// so `Enabled` is recognised as the free field it decodes to rather than being
// gated as an unknown.
func isFreeLSPField(key string) bool {
	for _, f := range policyLSPFreeFields {
		if strings.EqualFold(key, f) {
			return true
		}
	}
	return false
}

// lspPolicyKey splits an already-lowercased "lsp.<lang>.<field>" policy key. ok
// is false for any other key shape.
func lspPolicyKey(key string) (lang, field string, ok bool) {
	parts := strings.Split(key, ".")
	if len(parts) != 3 || parts[0] != "lsp" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// ProjectPolicySpecFor reads a workspace's project config and returns the
// capability-granting keys it sets. Mirrors ProjectTaskCommands: it reads the
// raw project TOML, so the set is provenance-filtered by construction — a value
// inherited from the global config or a compiled default is never in it, because
// only a project's own request needs trust.
func ProjectPolicySpecFor(workspace string) (ProjectPolicySpec, error) {
	raw, err := LoadProjectRaw(workspace)
	if err != nil {
		return nil, err
	}
	return projectPolicySpecFrom(raw), nil
}

// projectPolicySpecFrom extracts the capability-granting keys from an already
// parsed project config map. LoadProject uses this rather than
// ProjectPolicySpecFor so the spec and the merged config come from the SAME
// bytes: re-reading the file to compute the spec would open a window in which
// the file changes between the two reads and trust is checked against content
// that is not what was merged.
//
// Both tables are walked KEY BY KEY over what the project actually wrote, never
// by looking up an expected name. [git] is taken whole because every field in it
// is a safety decision; [lsp.<lang>] is taken whole minus the four provably inert
// fields (policyLSPFreeFields). Looking keys up by name is what made fold
// variants such as `Command` invisible to the spec while still decoding into
// LSPConfig — a key that is present but unrecognised must end up gated, not
// dropped.
//
// Keys keep the spelling the project used, so the disclosure shows what is
// really in the file and two spellings of one field cannot mask one another:
// both appear, and both are hashed.
func projectPolicySpecFrom(raw map[string]any) ProjectPolicySpec {
	var out ProjectPolicySpec
	for _, git := range rawTables(raw, "git") {
		for k, v := range git {
			out = append(out, PolicyEntry{Key: "git." + k, Value: v})
		}
	}
	for _, lsp := range rawTables(raw, "lsp") {
		for lang, langRaw := range lsp {
			table, ok := langRaw.(map[string]any)
			if !ok {
				continue
			}
			for k, v := range table {
				if isFreeLSPField(k) {
					continue
				}
				out = append(out, PolicyEntry{Key: "lsp." + lang + "." + k, Value: v})
			}
		}
	}
	for _, collab := range rawTables(raw, "collab") {
		for k, v := range collab {
			if isFreeCollabField(k) {
				continue
			}
			out = append(out, PolicyEntry{Key: "collab." + k, Value: v})
		}
	}
	out = append(out, execPolicyEntries(raw)...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// rawValues returns every top-level value in the parsed project config whose key
// matches want case-insensitively, WHATEVER its type.
//
// It returns a SLICE, not one value, because a lookup by exact name is a gate
// bypass. go-toml/v2 binds a table name to a struct field case-insensitively
// exactly as it does a field name, so `[COLLAB]` decodes into Config.Collab —
// but `raw["COLLAB"]` never matches an exact `raw["collab"]`, so those keys
// reach the merged config while being absent from the spec, the `plumb trust`
// disclosure, and the policy hash. A repository could then have a trusted
// `[git]` grant, append `[COLLAB] cross_project = true`, and be honoured without
// the hash changing — the precise TOCTOU the spec exists to close.
//
// TOML forbids defining the same table twice, but only for the same spelling:
// `[collab]` and `[COLLAB]` are two distinct tables to the parser and both land
// in the map, so every match must be walked rather than the first one taken.
//
// It is type-agnostic because not every gated section is a table. `[[command]]`
// is an ARRAY of tables, which decodes to []any; the same array written inline
// (`command = [{...}]`) decodes the same way, and a malformed `command = "x"`
// decodes to a string. A helper that only recognised map[string]any would drop
// all three from the spec — silently, which is the failure mode this whole file
// exists to avoid. Taking the value whatever its shape means an unexpected type
// is HASHED rather than ignored, so the gate fails closed on shapes nobody
// anticipated.
func rawValues(raw map[string]any, want string) []any {
	var out []any
	for k, v := range raw {
		if strings.EqualFold(k, want) {
			out = append(out, v)
		}
	}
	return out
}

// rawTables narrows rawValues to the matches that are tables, for the sections
// that are walked key by key.
func rawTables(raw map[string]any, want string) []map[string]any {
	var out []map[string]any
	for _, v := range rawValues(raw, want) {
		if table, ok := v.(map[string]any); ok {
			out = append(out, table)
		}
	}
	return out
}

// policyCollabFreeFields are the [collab] keys that are NOT gated on trust,
// because none of them can open a channel: peer_awareness surfaces only what the
// daemon already observed in THIS project, and the rest are sizes and expiries.
// max_exchanges remains here only as an ignored legacy key, so an existing
// project config does not start demanding trust after the limit's removal.
//
// Like policyLSPFreeFields this is an ALLOW-list, and for the same reason. The
// gated set is the interesting one, but enumerating IT would mean a [collab] key
// added later is silently free until someone remembers to classify it — which is
// exactly how the channel switches came to be project-settable in the first
// place. Enumerating what is safe makes the default "gated", so a new field
// fails closed and a human has to decide.
var policyCollabFreeFields = map[string]bool{
	"peer_awareness": true, "hint_budget_bytes": true, "intent_ttl_minutes": true,
	"max_exchanges": true, "chat_budget_bytes": true, "max_wait_seconds": true,
}

// IsGatedProjectKey reports whether a dotted TOML key is one LoadProject gates
// on trust. It exists so a display surface cannot drift from the loader: the TUI
// needs the same answer to decide how to present a row whose trust status could
// not be read, and duplicating the classification there is how the two get out of
// step.
func IsGatedProjectKey(dotted string) bool {
	key := strings.ToLower(dotted)
	switch {
	case strings.HasPrefix(key, "git."):
		return true
	case strings.HasPrefix(key, "lsp."):
		if _, field, ok := lspPolicyKey(key); ok {
			return !isFreeLSPField(field)
		}
		return true
	case strings.HasPrefix(key, "collab."):
		return !isFreeCollabField(strings.TrimPrefix(key, "collab."))
	case key == "command":
		return true
	case strings.HasPrefix(key, "commands."):
		return !isFreeCommandsField(strings.TrimPrefix(key, "commands."))
	case strings.HasPrefix(key, "xcode."):
		return true
	}
	return false
}

// isFreeCollabField reports whether a [collab] key is inert. Matched
// case-INSENSITIVELY, because go-toml/v2 binds a TOML key to a struct field that
// way: `Cross_Project = true` reaches CrossProject, and an exact-match check
// would let it past the gate unseen.
func isFreeCollabField(key string) bool { return policyCollabFreeFields[strings.ToLower(key)] }

// ProjectPolicyStatus is what a workspace's project config asks for in the
// capability-granting sections, and whether that exact request is trusted. It
// backs the visibility surfaces — a forced-back project config that says nothing
// is the same defect as a project config that is silently honoured, just in the
// other direction.
type ProjectPolicyStatus struct {
	// Path is the project config file the request came from ("" when there is
	// no workspace).
	Path string
	// Spec is the capability-granting request. Empty means there is nothing to
	// trust and nothing to report.
	Spec ProjectPolicySpec
	// Trusted reports whether this exact spec has been approved for this root.
	// Always false for an empty spec, which needs no approval.
	Trusted bool
}

// InEffect reports whether the project's capability-granting keys are actually
// applied. True when there are none (nothing was asked for) or when the exact
// request is trusted.
func (st ProjectPolicyStatus) InEffect() bool {
	return st.Spec.IsEmpty() || st.Trusted
}

// NeedsTrust reports whether the project asked for capability-granting settings
// that are currently being ignored — the state every visibility surface exists
// to announce.
func (st ProjectPolicyStatus) NeedsTrust() bool {
	return !st.Spec.IsEmpty() && !st.Trusted
}

// Asked reports whether the project config sets the given dotted key, so a
// per-row display (`plumb config show`) can distinguish "this is the value in
// effect" from "the project asked for something else here, untrusted".
//
// The comparison is case-insensitive because the spec stores the spelling the
// project used, while a caller asks with the canonical one — and the two differ
// for exactly the fold variants that must not slip past a display surface.
func (st ProjectPolicyStatus) Asked(key string) bool {
	for _, e := range st.Spec {
		if strings.EqualFold(e.Key, key) {
			return true
		}
	}
	return false
}

// ProjectPolicyStatusFor resolves the trust state of a workspace's
// capability-granting project config. A read error surfaces as an error; an
// unreadable trust store fails closed inside IsTrustedForPolicy.
func ProjectPolicyStatusFor(workspace string) (ProjectPolicyStatus, error) {
	spec, err := ProjectPolicySpecFor(workspace)
	if err != nil {
		return ProjectPolicyStatus{}, err
	}
	st := ProjectPolicyStatus{Path: ProjectConfigPath(workspace), Spec: spec}
	if !spec.IsEmpty() {
		st.Trusted = projectPolicyTrust().IsTrustedForPolicy(workspace, spec)
	}
	return st, nil
}

// projectPolicyTrust returns the trust store LoadProject consults.
//
// It is a package-level indirection rather than a LoadProject parameter on
// purpose. LoadProject has fifteen callers — the workspace pool that spawns the
// language server, `plumb config show`, the TUI settings editor, the web
// settings API, doctor — and every one of them must observe the same boundary.
// Threading a store through the signature would churn all fifteen and, worse,
// would let a future caller pass nil and quietly opt out of the gate. One
// chokepoint that cannot be bypassed is the whole point. Tests replace it via
// setProjectPolicyTrustForTest so they never touch the real <DataDir>/trust.json.
var projectPolicyTrust = func() *TrustStore { return NewTrustStore() }

// forceCapabilityFieldsToBase overwrites, in merged, every setting a project's
// .plumb/config.toml may set only with the user's explicit approval. It runs
// whenever the project's request is not trusted for this exact content, so the
// UNTRUSTED behaviour is byte-for-byte what it was before trust existed.
func forceCapabilityFieldsToBase(base Config, merged *Config) {
	// [git] is the git tool's tiered safety policy in its entirety — the gate that
	// decides whether the destructive tier (reset, clean, checkout, rebase) and the
	// network tier (push, fetch, pull) may run at all, and which branches can never
	// be force-pushed. The connection builds its live tools.GitPolicy straight from
	// this block, with no per-call trust check anywhere behind it. A cloned hostile
	// repo shipping allow_destructive = true, allow_push = true and an empty
	// protected_branches would therefore grant itself history destruction and
	// arbitrary pushes to the user's remotes, using the user's credentials, the
	// moment a session attaches. Forced back whole, never field by field —
	// which is also what gives [git] env (the git child's environment, a
	// code-execution channel in its own right) this boundary for free.
	merged.Git = base.Git
	// The assignment above shares base's map and the caller usually holds base in
	// a live config store, so break the alias (mirroring forceLSPExecToBase).
	// Cloning merged.Git.Env — which the line above has just made base's — rather
	// than base.Git.Env is deliberate: it leaves this line incapable of forcing
	// anything by itself, so a test asserting the env was forced back really
	// tests the whole-block reset above.
	merged.Git.Env = maps.Clone(merged.Git.Env)
	// [collab]'s four switches each open a cross-agent CHANNEL: share_intent's
	// agent-authored claims, the mailbox delivering a message to a peer or to
	// whoever attaches next, cross-project delivery, and share_findings writing
	// agent-authored content into the durable, cross-session-discoverable memory
	// store. A channel a cloned repository can open is a channel it can use — a
	// payload that has already steered one agent through some other file in the
	// repo can leave instructions for the next session.
	//
	// cross_project is the plainest case: its own contract is that receiving is the
	// RECIPIENT's decision rather than the sender's, which holds only while the
	// recipient's project file cannot set it unasked. Trust is what makes a
	// per-project value legitimate here — the user, not the repository, decides.
	//
	// Only the switches are gated. peer_awareness and the budgets stay freely
	// project-overridable: they surface what the daemon already observed in this
	// project, or tune a size, and neither can open anything.
	merged.Collab.Intents = base.Collab.Intents
	merged.Collab.Mailbox = base.Collab.Mailbox
	merged.Collab.CrossProject = base.Collab.CrossProject
	merged.Collab.KnowledgeHandoff = base.Collab.KnowledgeHandoff
	// [lsp.<lang>] command/args/env are the argv and environment of a process the
	// daemon spawns (pool → lsp.NewSupervisor → exec.CommandContext), so honouring
	// them from an untrusted project config is arbitrary code execution: cloning a
	// repo whose .plumb/config.toml says `command = "/bin/sh"` would run the
	// attacker's command as the user, unsandboxed, on attach. See policyLSPFields
	// for the field-by-field reasoning and for what a project may always override.
	merged.LSP = forceLSPExecToBase(base.LSP, merged.LSP)
}

// forceLSPExecToBase returns merged's per-language [lsp.<lang>] tables with every
// field in policyLSPFields taken from base (the trusted global config) rather
// than from the project's file. Everything else in each table survives, because
// none of it can change the process or its inputs.
//
// Values are cloned so the returned config never shares backing storage with
// base (LoadProject's caller usually holds base in a live config store).
func forceLSPExecToBase(base, merged map[string]LSPConfig) map[string]LSPConfig {
	if merged == nil {
		return nil
	}
	out := make(map[string]LSPConfig, len(merged))
	for name, lspCfg := range merged {
		trusted, ok := base[name]
		if !ok {
			continue
		}
		lspCfg.Command = trusted.Command
		lspCfg.Args = slices.Clone(trusted.Args)
		lspCfg.Env = maps.Clone(trusted.Env)
		lspCfg.InitializationOptions = maps.Clone(trusted.InitializationOptions)
		lspCfg.RootMarkers = slices.Clone(trusted.RootMarkers)
		lspCfg.WeakRootMarkers = slices.Clone(trusted.WeakRootMarkers)
		out[name] = lspCfg
	}
	return out
}

// dropUnknownLSPLanguages removes any [lsp.<lang>] table for a language the
// global config does not define. This is NOT part of the trust boundary and is
// applied trusted or not: plumb has no adapter for a language its global config
// never declared, so such a table can only add an unbound argv to the merged
// config with no path to a working language server. Trust widens what a project
// may configure about the servers the user has; it does not let a project invent
// one.
func dropUnknownLSPLanguages(base, merged map[string]LSPConfig) map[string]LSPConfig {
	if merged == nil {
		return nil
	}
	out := make(map[string]LSPConfig, len(merged))
	for name, lspCfg := range merged {
		if _, ok := base[name]; ok {
			out[name] = lspCfg
		}
	}
	return out
}
