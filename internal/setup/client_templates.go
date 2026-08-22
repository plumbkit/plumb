package setup

import (
	_ "embed"
	"strings"
)

// Per-client managed-block bodies, embedded as data files rather than string
// constants in code (PLAN-364 PR 2). Each is size-guarded to MaxTemplateLines
// by TestManagedBlock_ClientTemplateSizeGuard, and each is written so it holds
// even under a lean/allowlisted client config: `plumb setup gemini --lean` and
// `plumb setup codex --lean` write a client-side tool allowlist
// (tools.LeanToolNames — read_file/edit_file/write_file/transaction_apply/
// run_task/git/session_start, among others) that strips both the peer
// mailbox (leave_note/check_messages) and every symbol-scoped edit tool
// (replace_symbol_body, insert_before/after_symbol, ...). Since a template is
// fixed once written — v1 has no per-invocation "is --lean in force?"
// branching — the codex and gemini bodies below name ONLY tools inside that
// allowlist, so the claim holds whether or not --lean was actually passed.
// claude-code has no --lean flag, so its body is free to name the peer
// mailbox and the subagent session_start pointer.
var (
	//go:embed templates/claude-code.md
	claudeCodeTemplateRaw string
	//go:embed templates/codex.md
	codexTemplateRaw string
	//go:embed templates/gemini.md
	geminiTemplateRaw string
)

// ClientTemplates maps a setup client key (setupTarget.use — "claude-code",
// "codex", "gemini") to its own managed-block body. A client not present here
// has no per-client template yet and callers fall back to DefaultTemplate,
// same as before this map existed.
var ClientTemplates = map[string]string{
	"claude-code": strings.TrimRight(claudeCodeTemplateRaw, "\n"),
	"codex":       strings.TrimRight(codexTemplateRaw, "\n"),
	"gemini":      strings.TrimRight(geminiTemplateRaw, "\n"),
}

// TemplateForClient returns client's own managed-block body and true, or
// ("", false) when client has no per-client template registered.
func TemplateForClient(client string) (string, bool) {
	body, ok := ClientTemplates[client]
	return body, ok
}
