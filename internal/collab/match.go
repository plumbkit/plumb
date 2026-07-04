package collab

import (
	"path/filepath"
	"strings"
)

// MatchPath reports whether relPath (workspace-relative, slash-separated)
// matches any of the intent's path globs. An empty glob list means the intent is
// unscoped and matches nothing here — an unscoped intent is a broadcast shown in
// workspace_sessions / session_start, not a per-file write hint, so it never
// fires a path-matched write hint.
//
// Glob semantics mirror the memory store's paths matcher closely enough for
// steering: a slashless glob matches the path's basename anywhere in the tree
// (so "ratelimit*" attaches wherever such a file lives); a glob with a slash is
// matched against the full relative path; a trailing "/**" matches everything
// under a directory prefix.
func MatchPath(globs []string, relPath string) bool {
	relPath = filepath.ToSlash(relPath)
	for _, g := range globs {
		if matchGlob(strings.TrimSpace(g), relPath) {
			return true
		}
	}
	return false
}

func matchGlob(glob, path string) bool {
	if glob == "" {
		return false
	}
	// A "dir/**" prefix matches the directory and anything under it.
	if strings.HasSuffix(glob, "/**") {
		head := strings.TrimSuffix(glob, "/**")
		return path == head || strings.HasPrefix(path, head+"/")
	}
	// A slashless glob is matched against the basename anywhere in the tree.
	if !strings.Contains(glob, "/") {
		if ok, _ := filepath.Match(glob, filepath.Base(path)); ok {
			return true
		}
	}
	// Otherwise match against the full relative path (filepath.Match treats
	// "*" as not spanning "/", so "internal/tools/ratelimit*" matches
	// "internal/tools/ratelimit.go" but not a nested file).
	ok, _ := filepath.Match(glob, path)
	return ok
}
