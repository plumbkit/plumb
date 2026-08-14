package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// project_policy_warnings.go is the DISCLOSURE half of the project-config trust
// boundary: given a capability-granting key a project asked for, say in plain
// text what the repository would gain by having it honoured.
//
// It is deliberately separate from the extraction in project_policy.go. That
// file decides WHAT needs trust and must fail closed; this one decides how to
// explain it, and its failure mode is a user approving something they did not
// understand. `plumb trust` renders these strings immediately above the
// confirmation prompt, so they are the last thing between a hostile argv and a
// yes.
//
// Two rules hold throughout. A key the code does not recognise still warns — it
// reached the spec precisely because it is not on a free-list, so the honest
// answer is that plumb cannot vouch for it. And each warning is phrased as what
// the REPOSITORY gains, because that is what the user is being asked to approve.
//
// Concurrency: pure functions over immutable values.

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
	case "git.env":
		// Unconditional, and a name allowlist would be no substitute: the
		// dangerous set is open-ended and reaches into other tools' variables
		// (GOFLAGS=-toolexec=… reaches any `go` a hook invokes).
		return "these environment variables are set on every git process plumb spawns, including the one that runs this repository's hooks; " +
			"several of them (GIT_SSH_COMMAND, GIT_EXTERNAL_DIFF, GIT_PROXY_COMMAND, GIT_PAGER) name a command git will run"
	}
	if field, ok := strings.CutPrefix(key, "collab."); ok {
		return collabFieldWarning(field, e.Value)
	}
	if w := execFieldWarning(key, e.Value); w != "" {
		return w
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
	// A known switch set to false (or to a non-bool, which LoadProject will reject
	// anyway) grants nothing, so it needs no warning. An UNRECOGNISED key is the
	// opposite case: plumb cannot say what any value of it does, so it must warn
	// whatever the value is — which is why this check sits inside the known cases
	// rather than in front of them.
	on, _ := v.(bool)
	switch field {
	case "cross_project":
		if !on {
			return ""
		}
		return "lets this repository's sessions receive messages from agents in OTHER projects on this machine"
	case "mailbox":
		if !on {
			return ""
		}
		return "opens the agent-to-agent mailbox here — this repo's agents can send messages into other sessions"
	case "intents":
		if !on {
			return ""
		}
		return "lets this repository's agents broadcast claims that other sessions are shown"
	case "knowledge_handoff":
		if !on {
			return ""
		}
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

// execFieldWarning explains why a [[command]] / [commands] / [xcode] key grants
// capability. Split out from Warning to stay within the gocyclo-15 contract,
// mirroring lspFieldWarning and collabFieldWarning.
//
// Every one of these keys reaches a process spawn, so an unrecognised key under
// [commands] or [xcode] still warns — it is in the spec precisely because it is
// not on the free-list, and the honest thing to say is that plumb cannot vouch
// for it.
func execFieldWarning(key string, v any) string {
	if key == "command" {
		return "this is the [[command]] allow-list — each entry is a fixed argv run_command will execute as you"
	}
	if field, ok := strings.CutPrefix(key, "commands."); ok {
		switch field {
		case "allow_shell":
			if on, _ := v.(bool); !on {
				return ""
			}
			return "opens execute_shell_command for this repository — arbitrary shell, as you, with your environment"
		case "deny_network":
			if off, ok := v.(bool); ok && off {
				return ""
			}
			return "re-opens network access for shell commands; the sandbox is integrity-only, so reads stay permissive and a command could exfiltrate what it reads"
		default:
			return "a [commands] key plumb does not recognise as inert; it is gated because it may reach command execution"
		}
	}
	if field, ok := strings.CutPrefix(key, "xcode."); ok {
		if field == "auto_build_server" {
			if on, _ := v.(bool); !on {
				return ""
			}
			return "runs xcodebuild in this repository on attach — which runs this repository's own build"
		}
		return "an input to the xcodebuild invocation plumb runs when auto_build_server is on"
	}
	return ""
}
