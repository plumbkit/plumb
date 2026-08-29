// Package clienttemplates is the single source of the per-client instruction
// prose that plumb hands to an agent through several channels: the managed
// AGENTS.md/CLAUDE.md/GEMINI.md block internal/setup writes into a project
// (PLAN-364), and the MCP `initialize` response's `instructions` field
// internal/mcp renders per connection (PLAN-366). One doctrine — lane rules,
// the refuse-to-break-the-build pointer, the mailbox pointer, the subagent
// session_start hint — sized and delivered per channel from this one body of
// text, instead of each channel drifting its own.
//
// It lives at the Foundation layer (internal/arch/layers.go) — stdlib and
// `embed` only — so BOTH a Domain-layer package (internal/setup) and a
// Transport-layer package (internal/mcp) can import it without inverting the
// layering: Foundation sits below both.
package clienttemplates

import (
	_ "embed"
	"strings"
)

// Per-client instruction bodies, embedded as data files rather than string
// constants in code (PLAN-364 PR 2, relocated here by PLAN-366). Each is
// size-guarded to MaxLines by TestManagedBlock_ClientTemplateSizeGuard
// (internal/setup) and each holds even under a lean/allowlisted client
// config: `plumb setup gemini --lean` and `plumb setup codex --lean` write a
// client-side tool allowlist (tools.LeanToolNames — read_file/edit_file/
// write_file/transaction_apply/run_task/git/session_start, among others)
// that strips both the peer mailbox (leave_note/check_messages) and every
// symbol-scoped edit tool (replace_symbol_body, insert_before/after_symbol,
// ...). Since a template is fixed once rendered, the codex and gemini bodies
// below name ONLY tools inside that allowlist, so the claim holds whether or
// not --lean was actually passed. claude-code has no --lean flag, so its
// body is free to name the peer mailbox and the subagent session_start
// pointer.
var (
	//go:embed templates/claude-code.md
	claudeCodeRaw string
	//go:embed templates/codex.md
	codexRaw string
	//go:embed templates/gemini.md
	geminiRaw string
)

// ByClient maps a canonical client key (clientcaps.Capabilities.Name /
// setupTarget.use — "claude-code", "codex", "gemini") to its own instruction
// body. A client not present here has no per-client body yet; callers fall
// back to DefaultTemplate.
var ByClient = map[string]string{
	"claude-code": strings.TrimRight(claudeCodeRaw, "\n"),
	"codex":       strings.TrimRight(codexRaw, "\n"),
	"gemini":      strings.TrimRight(geminiRaw, "\n"),
}

// ForClient returns client's own instruction body and true, or ("", false)
// when client has no per-client body registered.
func ForClient(client string) (string, bool) {
	body, ok := ByClient[client]
	return body, ok
}
