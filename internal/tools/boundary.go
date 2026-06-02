package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BoundaryGuard rejects paths outside the workspace pinned to this MCP
// connection. A nil guard is a no-op, preserving simple unit-test setup.
type BoundaryGuard func(path string) error

type WorkspaceBoundaryError struct {
	Workspace string
	Path      string
}

func (e WorkspaceBoundaryError) Error() string {
	return fmt.Sprintf(
		"workspace boundary violation: this connection is pinned to %s; %s is in a different project. "+
			"To work there, call session_start with workspace set to that project's root — it will re-pin this connection. "+
			"Do not browse other projects on disk.",
		e.Workspace, e.Path,
	)
}

func (g BoundaryGuard) check(path string) error {
	if g == nil || path == "" {
		return nil
	}
	return g(path)
}

func cleanToolPath(path string) string {
	return strings.TrimPrefix(path, "file://")
}

func NewWorkspaceBoundaryError(workspace, path string) error {
	return WorkspaceBoundaryError{Workspace: workspace, Path: path}
}

// IsWorkspaceBoundaryError reports whether err (or anything wrapped in it via
// %w) is a WorkspaceBoundaryError. All call sites wrap with %w, so errors.As
// alone is the contract — do not add a substring fallback, as it would
// false-positive on unrelated errors that happen to echo the message.
func IsWorkspaceBoundaryError(err error) bool {
	var boundaryErr WorkspaceBoundaryError
	return errors.As(err, &boundaryErr)
}

// PathWithinWorkspace reports whether path stays inside workspace after best
// effort canonicalisation. It follows symlinks for existing paths and for the
// nearest existing ancestor, so a symlink inside the workspace cannot be used to
// escape the boundary when creating a new file below it.
func PathWithinWorkspace(workspace, path string) bool {
	if workspace == "" || path == "" {
		return true
	}
	return withinRoot(canonicalRoot(workspace), canonicalRoot(path))
}

// canonicalRoot resolves path for boundary comparison: symlinks are followed
// for the path and its nearest existing ancestor (so a not-yet-created file
// resolves against its real parent), falling back to a lexical clean when
// resolution fails entirely. Both PathPolicy roots and candidate paths pass
// through it, so matching is always on resolved paths.
func canonicalRoot(path string) string {
	if path == "" {
		return ""
	}
	if p, err := canonicalPathForBoundary(path); err == nil {
		return p
	}
	return filepath.Clean(cleanToolPath(path))
}

// withinRoot reports whether the already-canonicalised path lies within root
// (or is root itself). Both arguments must be resolved by canonicalRoot first.
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func canonicalPathForBoundary(path string) (string, error) {
	abs, err := filepath.Abs(cleanToolPath(path))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	dir := abs
	var suffix []string
	for {
		if _, err := os.Stat(dir); err == nil {
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(abs), nil
		}
		suffix = append(suffix, filepath.Base(dir))
		dir = parent
	}
}
