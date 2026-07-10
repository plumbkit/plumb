package cli

// boundary_failclosed_test.go — the fail-closed contract for an unattached
// connection, and the refusal to seed a workspace from a relative tool path.
//
// Before this, a connection with no pinned workspace had a nil PathPolicy and
// checkBoundary allowed every path. A relative path then reached the filesystem
// unresolved, and the OS anchored it to the daemon's working directory — a
// singleton process whose cwd belonged to whichever client happened to spawn it.
// The result was a silent write into an unrelated repository. These tests pin
// the refusal in place.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// newUnattachedSession builds a connSession that has never attached a workspace.
func newUnattachedSession(t *testing.T) *connSession {
	t.Helper()
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	if got := s.workspace(); got != "" {
		t.Fatalf("fresh session already pinned to %q", got)
	}
	return s
}

// TestCheckBoundary_UnattachedRefuses is the core fail-closed assertion: with no
// pinned workspace, every path — absolute or relative, read or write — is
// refused rather than allowed.
func TestCheckBoundary_UnattachedRefuses(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newUnattachedSession(t)

	cases := []struct {
		name string
		path string
		want tools.Access
	}{
		{name: "relative write", path: "Tests/PautaTests/Foo.swift", want: tools.AccessReadWrite},
		{name: "relative read", path: "README.md", want: tools.AccessRead},
		{name: "bare filename write", path: "Makefile", want: tools.AccessReadWrite},
		{name: "absolute write", path: "/Users/dev/other-project/main.go", want: tools.AccessReadWrite},
		{name: "absolute read", path: "/etc/passwd", want: tools.AccessRead},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.checkBoundary(c.path, c.want)
			if err == nil {
				t.Fatalf("checkBoundary(%q) allowed the path on an unattached session; it must fail closed", c.path)
			}
			var unattached tools.UnattachedWorkspaceError
			if !errors.As(err, &unattached) {
				t.Fatalf("checkBoundary(%q) = %v, want UnattachedWorkspaceError", c.path, err)
			}
			if !tools.IsWorkspaceBoundaryError(err) {
				t.Error("UnattachedWorkspaceError must satisfy IsWorkspaceBoundaryError so callers suppress their fallbacks")
			}
		})
	}
}

// TestCheckBoundary_UnattachedEmptyPathIsNoop keeps the "no path, nothing to
// check" contract: a tool passing "" (an optional path argument) is not refused.
func TestCheckBoundary_UnattachedEmptyPathIsNoop(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newUnattachedSession(t)

	if err := s.checkBoundary("", tools.AccessReadWrite); err != nil {
		t.Fatalf("empty path on an unattached session = %v, want nil", err)
	}
}

// TestCheckBoundary_UnattachedRefusalClearsOnAttach asserts the refusal is a
// condition of being unpinned, not a sticky state: pinning a workspace restores
// normal service for paths inside it.
func TestCheckBoundary_UnattachedRefusalClearsOnAttach(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newUnattachedSession(t)

	if err := s.checkBoundary("Makefile", tools.AccessReadWrite); err == nil {
		t.Fatal("unattached relative path was allowed")
	}

	root := t.TempDir()
	mkTestFile(t, filepath.Join(root, "go.mod"), "module x\n")
	s.attachWorkspace(context.Background(), "file://"+root)
	if got := s.workspace(); got != root {
		t.Fatalf("workspace = %q, want %q", got, root)
	}
	if err := s.checkBoundary(filepath.Join(root, "Makefile"), tools.AccessReadWrite); err != nil {
		t.Fatalf("path inside the freshly pinned workspace refused: %v", err)
	}
}

// TestCheckBoundary_AttachedAllowsInsideRefusesOutside is the regression guard
// for the ordinary attached case: the fail-closed change must neither loosen nor
// tighten the boundary for a pinned connection.
func TestCheckBoundary_AttachedAllowsInsideRefusesOutside(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newUnattachedSession(t)

	root := t.TempDir()
	mkTestFile(t, filepath.Join(root, "go.mod"), "module x\n")
	s.attachWorkspace(context.Background(), "file://"+root)

	if err := s.checkBoundary(filepath.Join(root, "sub", "a.go"), tools.AccessReadWrite); err != nil {
		t.Errorf("write inside the workspace refused: %v", err)
	}
	outside := t.TempDir()
	err := s.checkBoundary(filepath.Join(outside, "a.go"), tools.AccessReadWrite)
	if err == nil {
		t.Fatal("write outside the workspace allowed")
	}
	var boundary tools.WorkspaceBoundaryError
	if !errors.As(err, &boundary) {
		t.Errorf("outside-workspace error = %v, want WorkspaceBoundaryError", err)
	}
}

// TestOnBeforeTool_RelativeSeedDoesNotAttach proves the daemon never resolves a
// relative tool path against its own working directory to find a workspace. The
// cwd here is a real project — the shape of the bug in the field, where the
// daemon inherited the cwd of whichever client spawned it. A relative seed must
// leave the session unattached rather than adopting that stranger's repository.
func TestOnBeforeTool_RelativeSeedDoesNotAttach(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	other := t.TempDir()
	mkTestFile(t, filepath.Join(other, "go.mod"), "module other\n")
	t.Chdir(other)

	s := newUnattachedSession(t)
	args := json.RawMessage(`{"file_path":"Tests/PautaTests/Foo.swift"}`)
	s.onBeforeTool(context.Background(), "write_file", args)

	if got := s.workspace(); got != "" {
		t.Fatalf("a relative tool path attached the connection to %q (the daemon cwd); it must leave the session unattached", got)
	}
	if err := s.checkBoundary("Tests/PautaTests/Foo.swift", tools.AccessReadWrite); err == nil {
		t.Fatal("relative path on the still-unattached session was allowed")
	}
}

// TestOnBeforeTool_AbsoluteSeedStillAttaches guards the other side: refusing a
// relative seed must not break the ordinary auto-attach from an absolute tool
// path, which is how a client that reports no roots gets pinned at all.
func TestOnBeforeTool_AbsoluteSeedStillAttaches(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newUnattachedSession(t)

	root := t.TempDir()
	mkTestFile(t, filepath.Join(root, "go.mod"), "module x\n")
	src := filepath.Join(root, "main.go")
	mkTestFile(t, src, "package main\n")

	raw, err := json.Marshal(map[string]string{"file_path": src})
	if err != nil {
		t.Fatal(err)
	}
	s.onBeforeTool(context.Background(), "read_file", raw)

	if got := s.workspace(); got != root {
		t.Fatalf("absolute seed attached to %q, want %q", got, root)
	}
}

func mkTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
