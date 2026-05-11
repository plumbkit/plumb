package tools

import (
	"path/filepath"
	"strings"
)

// isInWorkspace reports whether uri's path lies inside workspace and is not
// in a common dependency-cache location (Go module cache, GOROOT). Returns
// true if workspace is empty (can't filter without a root, fall back to gopls).
//
// LSP servers (especially gopls workspace_symbols) sometimes return symbols
// from $GOPATH/pkg/mod, /usr/local/go/src, etc. when the query is broad.
// Filtering them out keeps results focused on the user's own code.
func isInWorkspace(uri, workspace string) bool {
	if workspace == "" {
		return true
	}
	path := strings.TrimPrefix(uri, "file://")

	// Reject obvious dependency paths regardless of workspace.
	switch {
	case strings.Contains(path, "/pkg/mod/"):
		return false
	case strings.Contains(path, "/go/libexec/"):
		return false
	case strings.HasPrefix(path, "/usr/local/go/src/"):
		return false
	case strings.HasPrefix(path, "/usr/lib/go/src/"):
		return false
	case strings.HasPrefix(path, "/opt/homebrew/Cellar/go/"):
		return false
	}

	cleaned := filepath.Clean(path)
	cleanedWs := filepath.Clean(workspace)
	return cleaned == cleanedWs || strings.HasPrefix(cleaned, cleanedWs+string(filepath.Separator))
}
