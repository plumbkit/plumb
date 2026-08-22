package tools

import (
	"fmt"
	"sort"
)

// LeanTools is the single source of truth for the tools advertised under the
// "lean" profile — the set a client that already has native filesystem and
// search tools keeps. Every other registered tool is a commodity duplicate
// hidden from tools/list yet still callable by name (hidden ≠ unregistered).
//
// MUTATION-LANE RULE: a read-only commodity tool (copy_file, find_files,
// the extra search/symbol conveniences) may be hidden freely, but a mutation
// tool whose native fallback is UNSAFE must stay lean — a client that falls back
// to shell mv/rm/sed bypasses plumb's per-path locks, the LSP
// didChangeWatchedFiles notify, and the transaction WAL. So write_file,
// edit_file, rename_file, delete_file, transaction_apply, and undo_edit are all
// lean.
// read_file and read_symbol also stay lean: the edit lane needs their mtime/sha
// headers and the ReadTracker hand-off, so hiding them would recreate the
// "has not been read" lane-mixing failure. rename_symbol stays lean because its
// only safe equivalent is itself.
//
// run_task is kept lean DELIBERATELY even though it is not a file/edit tool: it
// is the trust-gated, no-shell, bounded build/test/lint runner, and its only
// "native equivalent" is a raw shell `go test`/`zig build` — precisely the shell
// fallback plumb exists to replace. Hidden from tools/list, a recognised CLI
// client never SEES it and silently shells out to build, the exact anti-pattern
// the profile is meant to avoid. The read-only commodity search/list/find tools
// (search_in_files, find_files, file_diff, …) stay hidden under lean — a
// client that wants them sets [tools] profile = "full".
var LeanTools = map[string]bool{
	"session_start":     true,
	"read_file":         true,
	"read_symbol":       true,
	"file_outline":      true,
	"edit_file":         true,
	"write_file":        true,
	"rename_file":       true,
	"delete_file":       true,
	"transaction_apply": true,
	"undo_edit":         true,
	"git":               true,
	"diagnostics":       true,
	"get_definition":    true,
	"find_references":   true,
	"rename_symbol":     true,
	"workspace_symbols": true,
	"topology_search":   true,
	"topology_explore":  true,
	"topology_affected": true,
	"search_memories":   true,
	"run_task":          true,
}

// BootstrapTools is the minimal orientation surface every client must see in
// its INITIAL tools/list, whatever the resolved profile. session_start
// orients the agent in an unfamiliar workspace, git shows history/status,
// and read_file/edit_file are the read-before-write lane the edit contract
// depends on. A client that never sees session_start advertised has no
// reliable way to discover it exists, so session_start (and its bootstrap
// companions) must never become a hidden/deferred-only capability.
//
// Bootstrap membership is deliberately independent of LeanTools: today
// bootstrap ⊆ lean, but the two sets answer different questions ("what must
// always be visible" vs "what a lean client keeps") and must be free to
// diverge in future without silently breaking either guarantee — see
// TestBootstrapToolsAreLean, which pins today's containment as a reviewable
// invariant rather than an assumption baked into toolVisible.
var BootstrapTools = map[string]bool{
	"session_start": true,
	"git":           true,
	"read_file":     true,
	"edit_file":     true,
}

// IsBootstrap reports whether name is one of the always-visible bootstrap
// tools (see BootstrapTools).
func IsBootstrap(name string) bool { return BootstrapTools[name] }

// MailboxTools is the agent-to-agent mailbox PAIR. It is deliberately not part
// of LeanTools — a collaboration feature most sessions never use has no claim on
// a lean client's advertised surface — but both halves are pinned into the
// context of a client that supports pinning (mcp.Server.AlwaysLoad, see
// conn_register.go).
//
// WHY PIN A NON-LEAN PAIR. These are the only two tools in the registry whose
// own OUTPUT instructs the agent to call the other one: leave_note's reply tells
// the sender to read its mailbox, and check_messages' reply tells the reader to
// answer with leave_note. For an ordinary deferred tool the cost of deferral is
// one tool-search round-trip; for a half-finished exchange it is an instruction
// the agent has already been given and cannot follow. A real agent hit exactly
// that: it had leave_note, was told to call check_messages, and could not find
// it. Client-side schema deferral is the ONE mechanism that can split the pair,
// and pinning is its whole fix.
//
// THE PAIRING IS THE POINT. Add or remove both together — the asymmetry (send
// reachable, receive not) IS the defect, so a one-sided edit here recreates it.
// TestMailboxToolsArePairedAndNonLean pins that.
//
// No OTHER mechanism can produce that asymmetry, which is why nothing in the
// mailbox's prose is gated on reachability. Both names leave and enter together
// everywhere else: the lean profile hides both from tools/list (neither is in
// LeanTools), and a client-side allowlist — LeanToolNames(), what
// `plumb setup <client> --lean` writes — contains neither, so it strips both
// before a call could reach plumb. A rule that suppressed the name of one would
// only ever fire when the other was equally gone (nothing to render) or when
// both were present (a working pointer, needlessly withheld).
//
// Membership is NOT gated on [collab] mailbox. The tools are registered and
// advertised regardless of that flag — pinning changes only whether a schema is
// deferred, so gating it would leave both tools listed but their schemas hidden,
// which is strictly worse than the two schemas it saves. It would also make the
// pin depend on a hot-reloadable value read after tools/list was already sent.
var MailboxTools = map[string]bool{
	"leave_note":     true,
	"check_messages": true,
}

// IsMailbox reports whether name is one of the mailbox pair (see MailboxTools).
func IsMailbox(name string) bool { return MailboxTools[name] }

// PinnedTools is the explicit set of tools pinned into a Claude Code client's
// context via mcp.Server.AlwaysLoad (conn_register.go) — the tools whose full
// schema loads on every connection rather than being deferred behind an MCP
// tool-search round-trip.
//
// This answers a DIFFERENT question than LeanTools, and used to be conflated
// with it: AlwaysLoad was wired straight off IsLean(name) || IsBootstrap(name)
// || IsMailbox(name), a set derived to control tools/list VISIBILITY for a
// client with its own native tools, not schema-load order for a client whose
// tools/list is full but whose tool search only loads some schemas up front.
// The result was silent: workspace_search — the documented discovery entry
// point (session_start_guidance.go) — was never in LeanTools, so it was
// deferred on every full-profile Claude Code session and never discovered. A
// deferred tool has no schema in context, so an agent cannot call it and does
// not know it exists.
//
// The 20-tool set below is deliberately curated, not derived: session_start,
// read_file, read_symbol, file_outline, edit_file, write_file, git,
// diagnostics, workspace_search, search_in_files, get_definition,
// find_references, workspace_symbols, topology_search, topology_affected,
// transaction_apply, run_task, search_memories, leave_note, check_messages.
// It happens to be a superset of BootstrapTools and MailboxTools (both are
// pinned for their own stated reasons — see their doc comments), but is NOT
// defined in terms of them: growing or shrinking either set no longer silently
// changes what gets pinned.
//
// Evicted relative to the old LeanTools-derived pin: rename_file, delete_file,
// and undo_edit (reachable once edit_file itself is pinned and steers to
// them), topology_explore (reachable from topology_search output), and
// rename_symbol (17.2% advertisement-gate error rate in practice — re-pin once
// PLAN-363 improves that).
//
// NEVER PINNED (PLAN-367): read_multiple_files. It is a real turns win (one
// round trip instead of N, inline per-file errors), but a measured BYTE loss
// vs the same reads done individually — 76 bytes over three read_file calls
// on the docs/use-cases.md Scenario 10 sample, after two rounds of fixing a
// padded separator and a wrong byte count (PLAN-13, PLAN-357). Pinning it
// would put a tool that costs more tokens than the alternative into every
// Claude Code session's up-front context for free, which is the opposite of
// what pinning is for. Standing rule: don't pin a tool while its published
// number is a loss. Re-litigate only if a future measurement moves it to
// parity or a win — see docs/use-cases.md Scenario 10.
var PinnedTools = map[string]bool{
	"session_start":     true,
	"read_file":         true,
	"read_symbol":       true,
	"file_outline":      true,
	"edit_file":         true,
	"write_file":        true,
	"git":               true,
	"diagnostics":       true,
	"workspace_search":  true,
	"search_in_files":   true,
	"get_definition":    true,
	"find_references":   true,
	"workspace_symbols": true,
	"topology_search":   true,
	"topology_affected": true,
	"transaction_apply": true,
	"run_task":          true,
	"search_memories":   true,
	"leave_note":        true,
	"check_messages":    true,
}

// IsPinned reports whether name's schema is pinned into a Claude Code client's
// context on every connection (see PinnedTools).
func IsPinned(name string) bool { return PinnedTools[name] }

// LeanToolNames returns the sorted, deduplicated UNION of LeanTools and
// BootstrapTools. It is the single source of truth for a CLIENT-SIDE tool
// allowlist — the list `plumb setup <client> --lean` writes into the client's
// own MCP config: Kimi Code's mcp.json "enabledTools", Codex's "enabled_tools"
// on [mcp_servers.plumb], and Gemini CLI's "includeTools" on mcpServers.plumb.
// It is also the set session_start's guidance confines itself to for those
// clients (clientcaps.ClientSideAllowlist), since plumb cannot see whether the
// allowlist is in force and must be correct either way.
//
// The union, not LeanTools alone: a client-side allowlist is enforced by the
// CLIENT, so plumb's "bootstrap tools are advertised whatever the profile"
// guarantee cannot rescue a bootstrap tool the client itself filtered out. If
// the two sets ever diverge, an allowlist built from LeanTools alone could
// silently strip session_start. Today BootstrapTools ⊆ LeanTools
// (TestBootstrapToolsAreLean), so the union is exactly the lean set.
//
// Sorted so the written config is stable across runs — map iteration order is
// random, and an unsorted list would make every re-register look like a change
// and produce noisy diffs in a config the user may have in version control.
func LeanToolNames() []string {
	names := make([]string, 0, len(LeanTools)+len(BootstrapTools))
	seen := make(map[string]bool, len(LeanTools)+len(BootstrapTools))
	for _, set := range []map[string]bool{LeanTools, BootstrapTools} {
		for name, on := range set {
			if !on || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// IsLean reports whether name is advertised under the lean profile.
//
// This set no longer does double duty: it used to also be the backbone of
// mcp.Server.AlwaysLoad (Claude Code's pinned-schema set), which meant editing
// LeanTools silently moved both tools/list visibility and schema-pin behaviour
// together, and a tool answering "what does a lean client keep" is not
// necessarily the same tool as "what must never be deferred behind a
// ToolSearch round-trip" — see PinnedTools, which now answers that second
// question on its own terms.
func IsLean(name string) bool { return LeanTools[name] }

// ProfileNote is the terse session_start/orientation line reporting the
// resolved tool profile and the reason it was chosen (see autoProfileFor's
// stable kebab-case reasons: client-override, explicit-config,
// schema-discovery-only-client, verified-deferred-discovery,
// client-side-allowlist, unknown-client-baseline, unverified-deferred-discovery).
//
// Under "lean" it deliberately does NOT enumerate the hidden tools (they stay
// callable by name); hidden is the count suppressed from tools/list, folded in
// alongside the reason. Kept well under 256 bytes so it cannot dominate the
// session_start budget even at a three-digit hidden count.
//
// Under "full" with a non-empty reason it renders one compact line naming the
// reason. A "full" profile with an EMPTY reason is the legacy/unwired default
// (resolvedToolProfile's zero value) and produces no output at all, so a
// caller that never wires a profile accessor sees no behaviour change.
//
// IT REPORTS ADVERTISEMENT, NOT AVAILABILITY, and deliberately says nothing
// itself about a client-side allowlist — a --lean Codex user is told "full",
// which is exactly true (plumb advertised every tool) while their own config
// may have filtered the list afterwards, and plumb cannot observe that filter
// (clientcaps.ClientSideAllowlist) to say more here without guessing. The
// truthful caveat for THAT case is a separate, conditional sentence —
// ClientSideAllowlistNote, appended by writeSessionGuidance only for a client
// whose entry declares ClientSideAllowlist — rather than folded into this
// function, so a client with no such config keeps a clean profile line.
func ProfileNote(profile string, hidden int, reason string) string {
	if profile == "lean" {
		return fmt.Sprintf("Tool profile: lean — %d commodity tools hidden from "+
			"tools/list (still callable by name; set [tools] profile = \"full\" to "+
			"restore) (reason: %s).\n\n", hidden, reason)
	}
	if reason == "" {
		return ""
	}
	return fmt.Sprintf("Tool profile: full (reason: %s).\n\n", reason)
}

// ClientSideAllowlistNote is the honest caveat for a client whose clientcaps
// entry declares ClientSideAllowlist (Kimi Code, Codex, Gemini CLI): plumb
// SERVES the full advertised surface (ProfileNote's line is accurate on its
// own), but `plumb setup <client> --lean` can write a tool allowlist into the
// client's OWN config, which plumb cannot observe — tools/call arrives
// identically whether or not it is in force. The sentence therefore states
// the conditional truthfully instead of guessing which state this session is
// in, and points at the one command that actually knows (`plumb doctor`
// grades the allowlist's content against today's lean set — see
// docs/cli-reference.md's `--lean` row). The lean count is read live off
// LeanToolNames so a future lean-set change cannot make this sentence stale.
func ClientSideAllowlistNote() string {
	return fmt.Sprintf("Your client config may filter this further: if you ran "+
		"`plumb setup <client> --lean`, only the %d lean tools are actually loaded "+
		"regardless of what tools/list advertises above — run `plumb doctor` to check "+
		"what your config actually allows.\n\n", len(LeanToolNames()))
}
