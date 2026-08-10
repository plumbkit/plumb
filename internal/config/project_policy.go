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
// when it does not warrant one. It is domain knowledge (which key is dangerous),
// left for the caller to format — the same division as FlagsInlineInterpreter,
// which `plumb trust` already uses to warn about a task command.
func (e PolicyEntry) Warning() string {
	if lang, field, ok := lspPolicyKey(e.Key); ok {
		switch field {
		case "command", "args", "env":
			return "this is the argv/environment of a process plumb spawns as you, unsandboxed, on every attach"
		case "initialization_options":
			return "language servers treat initializationOptions as a command channel (rust-analyzer's check.overrideCommand runs an arbitrary argv; zls's enable_build_on_save runs this repo's build.zig)"
		case "root_markers", "weak_root_markers":
			return "this chooses whether the " + lang + " language server is elected for this repository"
		}
		return ""
	}
	switch e.Key {
	case "git.allow_destructive":
		if v, ok := e.Value.(bool); ok && v {
			return "opens the destructive git tier (reset, clean, checkout, rebase) for this repository"
		}
	case "git.allow_push":
		if v, ok := e.Value.(bool); ok && v {
			return "opens the network git tier (push, fetch, pull) — pushes use your credentials"
		}
	case "git.protected_branches":
		if a, ok := e.Value.([]any); ok && len(a) == 0 {
			return "empties the never-force-push list"
		}
	}
	return ""
}

// policyLSPFields are the per-language [lsp.<lang>] fields that decide WHICH
// process runs, or WITH WHAT. command/args/env are the literal argv and
// environment of the spawned language server (setting env is execution too: PATH
// re-points which binary a server invokes, DYLD_INSERT_LIBRARIES / LD_PRELOAD
// inject into it); initialization_options is passed verbatim to the server,
// which for real servers is a command channel; root_markers / weak_root_markers
// elect which installed server is bound to a directory. Every other field in the
// table (diagnostics, enabled, idle_timeout, max_workspaces) cannot change the
// process and so is freely project-overridable, trusted or not.
var policyLSPFields = []string{
	"command", "args", "env", "initialization_options", "root_markers", "weak_root_markers",
}

// lspPolicyKey splits a "lsp.<lang>.<field>" policy key. ok is false for any
// other key shape.
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
// The whole [git] table is taken key by key, including keys plumb does not
// recognise today: [git] is a safety policy, and a per-field exemption inside a
// safety policy is how the next hole is introduced.
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
			for _, field := range policyLSPFields {
				if v, present := table[field]; present {
					out = append(out, PolicyEntry{Key: "lsp." + lang + "." + field, Value: v})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

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
func (st ProjectPolicyStatus) Asked(key string) bool {
	for _, e := range st.Spec {
		if e.Key == key {
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
