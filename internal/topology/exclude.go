package topology

// exclude.go implements [topology] exclude_patterns — the one way to keep a
// COMMITTED tree out of the index.
//
// The setting was declared in config, listed in the agent-writable allowlist,
// and read by nothing: an agent could set it, be told the write succeeded, and
// have the index carry on regardless. Now that the indexer honours .gitignore,
// the redundant case (excluding a tree git already ignores) is handled for
// free, and what is left is the case nothing else covers — a vendored or
// generated tree the repository tracks on purpose.
//
// Matching reuses ignore.DoubleStarMatch, the same glob primitive .gitignore
// matching and find_files bottom out in, rather than introducing a third
// dialect for a user to guess at.

import (
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/ignore"
)

// matchesExcludePattern reports whether a workspace-relative path matches any
// configured exclude pattern.
//
// The rule is gitignore's, minus the negation and directory-only syntax: a
// pattern containing "/" is matched against the whole relative path, and one
// without is matched against the base name at any depth. "**" spans path
// components. Patterns are assumed already sanitised (see
// sanitizeExcludePatterns).
func matchesExcludePattern(patterns []string, rel string) bool {
	if len(patterns) == 0 {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		return false
	}
	base := path.Base(rel)
	for _, p := range patterns {
		if ignore.DoubleStarMatch(p, rel) {
			return true
		}
		if !strings.Contains(p, "/") && ignore.DoubleStarMatch(p, base) {
			return true
		}
	}
	return false
}

// catchAllExcludes are the patterns that would exclude the entire workspace.
//
// This is the guard the field's agent-writability requires. exclude_patterns is
// in agentWritableKeys, so an agent can set it on its own workspace; without
// this, `exclude_patterns = ["**"]` would prune every directory on the next
// resync, and pruneDeleted would then delete every row. Every topology tool
// would answer "nothing found" — successfully, with no error anywhere — and the
// config that did it is a file the agent wrote to itself. A wholesale exclusion
// is refused and logged rather than honoured; a caller that genuinely wants no
// index has `topology.enabled = false`, which says so.
var catchAllExcludes = map[string]bool{
	"*": true, "**": true, "**/*": true, "*/**": true, ".": true, "./**": true,
}

// sanitizeExcludePatterns drops empty and workspace-wide entries, logging each
// refusal once at construction rather than on every path tested.
func sanitizeExcludePatterns(workspace string, patterns []string) []string {
	if len(patterns) == 0 {
		return nil
	}
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if catchAllExcludes[strings.Trim(filepath.ToSlash(trimmed), "/")] {
			slog.Warn("topology: refusing exclude_patterns entry that would exclude the whole workspace; use [topology] enabled = false instead",
				"workspace", workspace, "pattern", trimmed)
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
