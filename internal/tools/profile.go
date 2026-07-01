package tools

import "fmt"

// LeanTools is the single source of truth for the tools advertised under the
// "lean" profile — the set a client that already has native filesystem and
// search tools keeps. Every other registered tool is a commodity duplicate
// hidden from tools/list yet still callable by name (hidden ≠ unregistered).
//
// MUTATION-LANE RULE: a read-only commodity tool (copy_file, list_directory,
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
// (search_in_files, find_files, list_directory, …) stay hidden under lean — a
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

// IsLean reports whether name is advertised under the lean profile.
//
// Double duty: this same set is also the "always loaded" set wired into
// mcp.Server.AlwaysLoad (see conn_register.go) — the tools plumb pins into a
// Claude Code client's context so MCP tool search never defers them behind a
// ToolSearch round-trip. Editing LeanTools moves BOTH behaviours; that is
// intentional ("the tools that matter most" is one list, not two).
func IsLean(name string) bool { return LeanTools[name] }

// LeanProfileNote is the terse session_start line shown when the lean profile is
// active. It deliberately does NOT enumerate the hidden tools (they stay
// callable by name); hidden is the count suppressed from tools/list. Kept well
// under 256 bytes so it cannot dominate the session_start budget.
func LeanProfileNote(hidden int) string {
	return fmt.Sprintf("Tool profile: lean — %d commodity tools hidden from "+
		"tools/list (still callable by name; set [tools] profile = \"full\" to "+
		"restore).\n\n", hidden)
}
