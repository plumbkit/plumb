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
}

// IsLean reports whether name is advertised under the lean profile.
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
