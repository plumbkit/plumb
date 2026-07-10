package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// BoundaryGuard rejects paths outside the workspace pinned to this MCP
// connection. A nil guard is a no-op, preserving simple unit-test setup.
type BoundaryGuard func(path string) error

type WorkspaceBoundaryError struct {
	Workspace    string
	Path         string
	ReadOnlyRoot string // non-empty when the path is under a read-only root; indicates a write was attempted
}

// UnattachedWorkspaceError is returned when a path-bearing tool is called on a
// connection with no pinned workspace. plumb refuses such a call rather than
// resolving the path: with no workspace there is no allowlist to check against,
// and a relative path would be resolved by the OS against the daemon's working
// directory — a singleton process whose cwd belongs to whichever client happened
// to spawn it, i.e. an unrelated repository. Fail closed: a refused call is
// recoverable, a misplaced write is not.
type UnattachedWorkspaceError struct {
	Path string
}

func (e UnattachedWorkspaceError) Error() string {
	return fmt.Sprintf(
		"no workspace is pinned to this connection, so %s was refused rather than resolved. "+
			"Call session_start with `workspace` set to an absolute project root to pin this connection, then retry. "+
			"If this session was working a moment ago, the daemon may have restarted and the pin was not re-established.",
		e.Path,
	)
}

func (e WorkspaceBoundaryError) Error() string {
	if e.ReadOnlyRoot != "" {
		return fmt.Sprintf(
			"path access denied: %s is under a read-only root (%s) and cannot be modified. "+
				"Dependency source is not editable; copy the file into your workspace to make changes.",
			e.Path, e.ReadOnlyRoot,
		)
	}
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
	return paths.URIToPath(path)
}

func NewWorkspaceBoundaryError(workspace, path string) error {
	return WorkspaceBoundaryError{Workspace: workspace, Path: path}
}

// IsWorkspaceBoundaryError reports whether err (or anything wrapped in it via
// %w) is a path-access refusal — either a WorkspaceBoundaryError (the path lies
// outside the connection's allowed roots) or an UnattachedWorkspaceError (there
// are no allowed roots because nothing is pinned). Callers use it to suppress a
// fallback that would re-attempt the same refused path. All call sites wrap with
// %w, so errors.As alone is the contract — do not add a substring fallback, as
// it would false-positive on unrelated errors that happen to echo the message.
func IsWorkspaceBoundaryError(err error) bool {
	var boundaryErr WorkspaceBoundaryError
	if errors.As(err, &boundaryErr) {
		return true
	}
	var unattachedErr UnattachedWorkspaceError
	return errors.As(err, &unattachedErr)
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
