package config

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
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

// Warning returns a short, plain-text reason this entry grants capability, or ""
// when it does not warrant one. base is the trusted global config, needed to
// judge a value that is dangerous only relative to what it replaces.
//
// The key is compared case-INSENSITIVELY. go-toml/v2 matches a TOML key to a
// struct tag case-insensitively, so `[git] Allow_Push = true` reaches
// GitConfig.AllowPush and is captured by the whole-table extraction — an exact
// `switch e.Key` would print it with no warning at all.
//
// It is domain knowledge (which key is dangerous), left for the caller to format
// — the same division as FlagsInlineInterpreter, which `plumb trust` already
// uses to warn about a task command.
func (e PolicyEntry) Warning(base Config) string {
	key := strings.ToLower(e.Key)
	if lang, field, ok := lspPolicyKey(key); ok {
		return lspFieldWarning(lang, field)
	}
	switch key {
	case "git.allow_destructive":
		if v, ok := e.Value.(bool); ok && v {
			return "opens the destructive git tier (reset, clean, checkout, rebase) for this repository"
		}
	case "git.allow_push":
		if v, ok := e.Value.(bool); ok && v {
			return "opens the network git tier (push, fetch, pull) — pushes use your credentials"
		}
	case "git.protected_branches":
		return droppedBranchWarning(base.Git.ProtectedBranches, e.Value)
	}
	if field, ok := strings.CutPrefix(key, "collab."); ok {
		return collabFieldWarning(field, e.Value)
	}
	return ""
}

// collabFieldWarning explains why a gated [collab] key grants capability. Split
// out from Warning to stay within the gocyclo-15 contract, mirroring
// lspFieldWarning.
//
// A field this does not recognise still warns: it reached the spec because it is
// NOT one of the inert keys (see policyCollabFreeFields), so the honest thing to
// say is that plumb cannot vouch for it. Each warning is phrased as what the
// repository GAINS, since that is what the user is being asked to approve.
func collabFieldWarning(field string, v any) string {
	on, _ := v.(bool)
	if !on {
		return "" // turning a channel off grants nothing
	}
	switch field {
	case "cross_project":
		return "lets this repository's sessions receive messages from agents in OTHER projects on this machine"
	case "mailbox":
		return "opens the agent-to-agent mailbox here — this repo's agents can send messages into other sessions"
	case "intents":
		return "lets this repository's agents broadcast claims that other sessions are shown"
	case "knowledge_handoff":
		return "lets this repository's agents write durable, cross-session-discoverable memories"
	default:
		return "a [collab] key plumb does not recognise as inert; it is gated because it may open a cross-agent channel"
	}
}

// lspFieldWarning explains why a gated [lsp.<lang>] field grants capability. A
// field this does not recognise still returns a warning: it reached the spec
// because it is NOT one of the four provably inert keys, so the honest thing to
// say is that plumb cannot vouch for it (see projectLSPPolicyKeys).
func lspFieldWarning(lang, field string) string {
	switch field {
	case "command", "args", "env":
		return "this is the argv/environment of a process plumb spawns as you, unsandboxed, on every attach"
	case "initialization_options":
		return "language servers treat initializationOptions as a command channel (rust-analyzer's check.overrideCommand runs an arbitrary argv; zls's enable_build_on_save runs this repo's build.zig)"
	case "root_markers", "weak_root_markers":
		return "this chooses whether the " + lang + " language server is elected for this repository"
	default:
		return "an [lsp." + lang + "] key plumb does not recognise as safe; it is gated because it may reach the language server's argv or environment"
	}
}

// droppedBranchWarning reports whether a project-supplied protected_branches list
// REMOVES a branch the global config protects.
//
// Warning only on an empty list was the bug: protected_branches is the complete
// protected set (see the git tool's force-push check), so
// `protected_branches = ["placeholder"]` unprotects main and master exactly as
// thoroughly as `[]` does, while looking like a considered value.
func droppedBranchWarning(global []string, v any) string {
	list, ok := v.([]any)
	if !ok {
		return ""
	}
	kept := make(map[string]bool, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			kept[strings.ToLower(s)] = true
		}
	}
	var dropped []string
	for _, b := range global {
		if !kept[strings.ToLower(b)] {
			dropped = append(dropped, b)
		}
	}
	if len(dropped) == 0 {
		return ""
	}
	return "removes " + strings.Join(dropped, ", ") + " from the never-force-push list"
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
	if git, ok := raw["git"].(map[string]any); ok {
		for k, v := range git {
			out = append(out, PolicyEntry{Key: "git." + k, Value: v})
		}
	}
	if lsp, ok := raw["lsp"].(map[string]any); ok {
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
	if collab, ok := raw["collab"].(map[string]any); ok {
		for k, v := range collab {
			if isFreeCollabField(k) {
				continue
			}
			out = append(out, PolicyEntry{Key: "collab." + k, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// policyCollabFreeFields are the [collab] keys that are NOT gated on trust,
// because none of them can open a channel: peer_awareness surfaces only what the
// daemon already observed in THIS project, and the rest are sizes and expiries.
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
	// moment a session attaches. Forced back whole, never field by field.
	merged.Git = base.Git
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

// policyDisplay renders a decoded TOML value in a compact TOML-ish form for the
// disclosure surfaces. Strings are quoted so an argument containing a space, or
// an empty one, is visible as such.
func policyDisplay(v any) string {
	switch t := v.(type) {
	case string:
		return strconv.Quote(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = policyDisplay(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + " = " + policyDisplay(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprintf("%v", t)
	}
}
