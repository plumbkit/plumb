package mcp

import (
	"context"
	"fmt"
	"strings"
)

// Tool-coverage groups for the self-test. Every tool plumb registers must
// appear in exactly one group; the union is the contract the integration
// harness asserts against the live tools/list (cmd/smoke). When a new tool is
// added, add its name here in the matching tier or the parity guard fails.
//
// The grouping doubles as the playbook's structure: each tier is exercised the
// same way (read-only, sandbox file, in-module scratch, or deferred), so the
// agent can follow one recipe per group.
var (
	selftestBootstrap = []string{"session_start", "version", "daemon_info"}

	selftestLSPQuery = []string{
		"find_symbol", "workspace_symbols", "get_definition", "explain_symbol",
		"list_symbols", "file_outline", "find_references", "call_hierarchy",
		"type_hierarchy", "diagnostics",
	}

	selftestReads = []string{
		"read_file", "read_symbol", "read_multiple_files", "list_directory",
		"list_files", "find_files", "search_in_files",
	}

	selftestGitRead = []string{"git"}

	selftestTopology = []string{
		"topology_status", "topology_search", "topology_explore",
		"topology_impact", "topology_affected", "topology_routes",
		"structural_query", "workspace_search",
	}

	selftestMemoryRead = []string{
		"list_memories", "read_memory", "search_memories", "relevant_memories",
	}

	selftestFSWrite = []string{
		"write_file", "edit_file", "copy_file", "rename_file", "delete_file",
		"transaction_apply", "find_replace", "file_diff",
	}

	selftestMemoryWrite = []string{"write_memory", "delete_memory"}

	selftestSession = []string{"rename_session", "workspace_sessions"}

	selftestSymbolEdit = []string{
		"rename_symbol", "replace_symbol_body", "insert_before_symbol",
		"insert_after_symbol", "safe_delete_symbol",
	}

	// selftestHarnessOnly names tools (and behaviours) that are unsafe or
	// non-deterministic to drive against the live workspace, so the agent does
	// not run git_init live; it creates a repo and is covered by the integration
	// harness (cmd/smoke). Additional smoke tests cover tool-list parity, strict
	// mode rejection, transaction rollback, and the allowed destructive reset tier.
	selftestHarnessOnly = []string{"git_init"}
)

// selftestToolNames is the canonical flat list of every tool the self-test
// system covers (live playbook + integration harness). The integration parity
// guard asserts this equals the live tools/list set.
func selftestToolNames() []string {
	groups := [][]string{
		selftestBootstrap, selftestLSPQuery, selftestReads, selftestGitRead,
		selftestTopology, selftestMemoryRead, selftestFSWrite,
		selftestMemoryWrite, selftestSession, selftestSymbolEdit,
		selftestHarnessOnly,
	}
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// SelftestToolNames returns a copy of the canonical tool-coverage list. Exported
// for the integration harness (cmd/smoke), which compares it against the live
// tools/list to catch a tool added without a checklist entry.
func SelftestToolNames() []string {
	names := selftestToolNames()
	out := make([]string, len(names))
	copy(out, names)
	return out
}

// SelftestPrompt returns a playbook that drives every registered tool against
// the live workspace, in a disposable sandbox, and reports a PASS/FAIL/SKIP
// table. It is the real-world complement to the cmd/smoke integration test:
// the actual agent exercises the actual tools, so it catches description and
// ergonomics problems a wire-level test cannot.
type SelftestPrompt struct{ workspacePrompt }

// NewSelftestPrompt constructs the self-test prompt. workspaceFn is the lazy
// resolver shared with the other workspace-aware prompts.
func NewSelftestPrompt(workspaceFn func() string) *SelftestPrompt {
	return &SelftestPrompt{workspacePrompt{
		name:        "selftest",
		description: "Run a self-test of every plumb tool against a disposable sandbox, then report PASS/FAIL/SKIP.",
		workspaceFn: workspaceFn,
	}}
}

func (p *SelftestPrompt) Arguments() []PromptArgument { return wsArg() }

// Expand assembles the playbook. It is a thin orchestrator over per-section
// builders so each stays simple and individually editable.
func (p *SelftestPrompt) Expand(_ context.Context, args map[string]string) ([]PromptMessage, error) {
	ws := resolveWS(args, p.workspaceFn)

	var b strings.Builder
	for _, section := range [][]string{
		selftestIntro(),
		selftestPreflight(ws),
		selftestSandboxSetup(),
		selftestTierA(),
		selftestTierB(),
		selftestTierC(),
		selftestDeferred(),
		selftestCleanup(),
		selftestReport(),
	} {
		b.WriteString(strings.Join(section, "\n"))
		b.WriteString("\n\n")
	}

	return []PromptMessage{
		{Role: "user", Content: PromptContent{Type: "text", Text: strings.TrimSpace(b.String())}},
	}, nil
}

// toolList renders a group as a back-ticked, comma-separated line. Every tool
// name therefore appears verbatim in the playbook, which the integrity test
// (TestSelftestPrompt_CoversEveryTool) relies on.
func toolList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	return strings.Join(quoted, ", ")
}

func selftestIntro() []string {
	return []string{
		"# plumb self-test",
		"",
		"Exercise every plumb tool against **this live workspace** and report which work.",
		"This is a real-world check: you (the agent) drive the real tools, so it surfaces",
		"confusing descriptions or awkward parameters a wire-level test never sees.",
		"",
		"**Rules:**",
		"- All destructive operations stay inside a disposable sandbox you create below.",
		"- Never edit, move, or delete a file you did not create during this run.",
		"- Clean up everything at the end, even if a step fails (final section is mandatory).",
		"- For each tool, record PASS / FAIL / SKIP(reason) — do not stop on the first failure.",
	}
}

func selftestPreflight(ws string) []string {
	call := "session_start({})"
	if ws != "" {
		call = fmt.Sprintf("session_start({\"workspace\": %q})", ws)
	}
	return []string{
		"## 0. Preflight",
		"",
		fmt.Sprintf("Call `%s`.", call),
		"The first call may take up to ~2 minutes while the language server cold-starts —",
		"this is expected; do not abort. Record from the output: the **workspace root**, the",
		"**language**, and whether **topology** is enabled (a topology line appears when it is).",
		"This step covers " + toolList(selftestBootstrap) + " (also call `version` and `daemon_info`).",
	}
}

func selftestSandboxSetup() []string {
	return []string{
		"## 1. Sandbox setup",
		"",
		"- **Filesystem sandbox:** create `selftest-sandbox/` under the workspace root and put",
		"  all Tier B files there. Everything in it is deleted at cleanup.",
		"- **Scratch package (Tier C only):** the language server only indexes files inside the",
		"  module/source tree, so symbol-edit tools need a real, compilable source file in a",
		"  package — not under a dot-dir. If the workspace language has an LSP (e.g. Go), create",
		"  one disposable source file in a new package directory and remember to delete it.",
		"  If the language has no LSP, SKIP Tier C with that reason.",
	}
}

func selftestTierA() []string {
	return []string{
		"## 2. Tier A — read-only (no mutation)",
		"",
		"Call each against existing content and check the shape of the result.",
		"",
		"- **LSP queries:** " + toolList(selftestLSPQuery) + ". First use `workspace_symbols`",
		"  to find a real symbol, then drive the position/name tools against it. If the language",
		"  server is unavailable, note whether the documented topology fallback engaged.",
		"- **Reads:** " + toolList(selftestReads) + ". Read a known existing file; confirm",
		"  `read_file` emits the `# plumb-read mtime=…` header.",
		"- **Git (read-only):** " + toolList(selftestGitRead) + " with `status`, then `log`",
		"  `[\"-3\", \"--oneline\"]`. Do not run write/destructive subcommands here.",
		"- **Topology:** " + toolList(selftestTopology) + ". If topology is disabled, SKIP all",
		"  with reason \"topology disabled\".",
		"- **Memory (read):** " + toolList(selftestMemoryRead) + ".",
	}
}

func selftestTierB() []string {
	return []string{
		"## 3. Tier B — filesystem sandbox",
		"",
		"Every operation targets a file you just created in `selftest-sandbox/`.",
		"",
		"- " + toolList(selftestFSWrite) + ": `write_file` a new file → `read_file` it (capture",
		"  the mtime) → `edit_file` with `expected_mtime` → `copy_file` it → `rename_file` the",
		"  copy → `file_diff` the two → `find_replace` (dry-run first, then apply) →",
		"  `transaction_apply` a small multi-file edit (happy path) → `delete_file` each.",
		"- **Memory (write):** " + toolList(selftestMemoryWrite) + ": `write_memory` a temp",
		"  memory named `selftest-temp`, confirm it, then `delete_memory` it.",
		"- **Session:** " + toolList(selftestSession) + ": read the current name via `daemon_info`,",
		"  `rename_session` to a temp name, then rename it back.",
	}
}

func selftestTierC() []string {
	return []string{
		"## 4. Tier C — symbol edits (LSP languages only)",
		"",
		"Against the disposable scratch source file from step 1 — never real source:",
		"",
		"- " + toolList(selftestSymbolEdit) + ": add a symbol, `insert_before_symbol` /",
		"  `insert_after_symbol` around it, `replace_symbol_body`, `rename_symbol`, then",
		"  `safe_delete_symbol` (it should refuse if external references exist — that is a PASS).",
		"- Delete the scratch file and its package directory when done.",
		"- If the workspace has no language server, SKIP this whole tier with that reason.",
	}
}

func selftestDeferred() []string {
	return []string{
		"## 5. Deferred to the integration harness (do NOT run live)",
		"",
		"These are unsafe or non-deterministic against a live workspace and are covered by",
		"`cmd/smoke` instead — mark them SKIP(\"integration harness\"):",
		"",
		"- " + toolList(selftestHarnessOnly) + " (creates a repo).",
		"- Smoke tests also cover `transaction_apply` rollback on partial failure, strict-mode",
		"  rejection, tool-list parity, and the allowed destructive `git reset` tier.",
	}
}

func selftestCleanup() []string {
	return []string{
		"## 6. Cleanup (MANDATORY — run even if steps failed)",
		"",
		"- `delete_file` every file you created; remove `selftest-sandbox/` and the scratch",
		"  package directory.",
		"- `delete_memory` the `selftest-temp` memory if it still exists.",
		"- Restore the original session name.",
		"- Confirm `git status` shows no leftover sandbox or scratch paths.",
	}
}

func selftestReport() []string {
	return []string{
		"## 7. Report",
		"",
		"Print a markdown table with columns **Tool | Tier | Result | Note**, one row per tool,",
		"using PASS / FAIL / SKIP(reason). End with a one-line summary: counts of each result",
		"and a flat list of any FAILs. If a tool's description or parameters were confusing or",
		"the output was hard to parse, call that out explicitly — that feedback is the point.",
	}
}
