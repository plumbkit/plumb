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
	return fmt.Sprintf("workspace boundary violation: connection is pinned to %s; refusing path %s outside that workspace", e.Workspace, e.Path)
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

func IsWorkspaceBoundaryError(err error) bool {
	var boundaryErr WorkspaceBoundaryError
	return err != nil && (errors.As(err, &boundaryErr) || strings.Contains(err.Error(), "workspace boundary violation"))
}

// PathWithinWorkspace reports whether path stays inside workspace after best
// effort canonicalisation. It follows symlinks for existing paths and for the
// nearest existing ancestor, so a symlink inside the workspace cannot be used to
// escape the boundary when creating a new file below it.
func PathWithinWorkspace(workspace, path string) bool {
	if workspace == "" || path == "" {
		return true
	}
	ws, err := canonicalExistingPath(workspace)
	if err != nil {
		ws = filepath.Clean(workspace)
	}
	p, err := canonicalPathForBoundary(path)
	if err != nil {
		p = filepath.Clean(path)
	}
	rel, err := filepath.Rel(ws, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func canonicalExistingPath(path string) (string, error) {
	abs, err := filepath.Abs(cleanToolPath(path))
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
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
