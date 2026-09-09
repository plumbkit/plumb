// Package clienttemplates is the single source of the per-client instruction
// prose that plumb hands to an agent: one doctrine — lane rules, the
// refuse-to-break-the-build pointer, the mailbox pointer, the subagent
// session_start hint — rendered into the MCP `initialize` response's
// `instructions` field by internal/mcp, per connection (PLAN-366).
//
// This is the ONLY channel. PLAN-364 also wrote these bodies into a managed
// block inside a project's own AGENTS.md/CLAUDE.md/GEMINI.md; that writer is
// gone, because plumb does not write prose into files it does not own. A
// body here must therefore describe what plumb is and what it needs, and must
// never instruct an agent to create a file on plumb's behalf.
//
// It lives at the Foundation layer (internal/arch/layers.go) — stdlib and
// `embed` only. Only the Transport-layer internal/mcp imports it today, so
// Foundation is lower than it strictly needs to be; it stays there because
// the layering rule forbids Transport importing Domain, and nothing here
// needs anything above Foundation to remain correct.
package clienttemplates

import (
	_ "embed"
	"strings"
)

// Per-client instruction bodies, embedded as data files rather than string
// constants in code (PLAN-364 PR 2, relocated here by PLAN-366). Each is
// size-guarded by internal/mcp's MaxInstructionsBytes
// (TestInstructions_KnownClientsWithinBudget) and each holds even under a
// lean/allowlisted client
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
