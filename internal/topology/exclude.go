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
// Matching and the catch-all guard both live in internal/ignore, next to
// DoubleStarMatch — the same glob primitive .gitignore matching and find_files
// bottom out in, rather than a third dialect for a user to guess at. Keeping
// the guard beside the matcher is what stops it drifting from the behaviour it
// guards, and lets internal/config refuse a bad pattern on the WRITE path
// without importing this package.

import (
	"log/slog"
	"strings"

	"github.com/plumbkit/plumb/internal/ignore"
)

// matchesExcludePattern reports whether a workspace-relative path matches any
// configured exclude pattern. Patterns are assumed already sanitised (see
// sanitizeExcludePatterns).
func matchesExcludePattern(patterns []string, rel string) bool {
	for _, p := range patterns {
		if ignore.MatchExcludePattern(p, rel) {
			return true
		}
	}
	return false
}

// sanitizeExcludePatterns drops empty entries and the ones
// ignore.ExcludePatternRefusal refuses, logging each refusal once at
// construction rather than on every path tested.
//
// This is the guard the field's agent-writability requires. exclude_patterns is
// in agentWritableKeys, so an agent can set it on its own workspace; without
// this, `exclude_patterns = ["**"]` would prune every directory on the next
// resync, and pruneDeleted would then delete every row. Every topology tool
// would answer "nothing found" — successfully, with no error anywhere — and the
// config that did it is a file the agent wrote to itself.
//
// It is the SECOND line, not the only one: config.validate refuses the same
// patterns on the write path, so an agent that sets one is told rather than
// warned about in a log it never reads. This one still stands, because a
// Config can also be built in code, and a warn-and-drop here keeps a single bad
// entry from taking down a store that would otherwise open.
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
		if why := ignore.ExcludePatternRefusal(trimmed); why != "" {
			slog.Warn("topology: refusing exclude_patterns entry",
				"workspace", workspace, "pattern", trimmed, "reason", why)
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
