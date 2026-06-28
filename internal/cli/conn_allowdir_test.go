package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// TestAllowDir_PerConnectionIsolation proves a read-write root granted to one
// connection via serve --allow-dir (onAllowDirs) never leaks into another
// connection attached to the same workspace. Connection A is granted allowDir;
// connection B is not. A may write under allowDir; B is refused by its
// PathPolicy — the per-session-isolation contract.
func TestAllowDir_PerConnectionIsolation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()

	ws := freshTempDir(t)
	mustGitDir(t, ws)
	allowDir := freshTempDir(t) // outside the workspace
	target := filepath.Join(allowDir, "file.txt")

	// Connection A: grant the allow-dir (as serve transports it during initialize),
	// then attach the workspace (as OnInit does after).
	sessA := newConnSession(context.Background(), pool, nil, store, nil, newSharedBudgets())
	defer sessA.close()
	sessA.onAllowDirs([]string{allowDir})
	sessA.attachWorkspace(context.Background(), "file://"+ws)

	// Connection B: same workspace, no allow-dir.
	sessB := newConnSession(context.Background(), pool, nil, store, nil, newSharedBudgets())
	defer sessB.close()
	sessB.attachWorkspace(context.Background(), "file://"+ws)

	// A may write under the granted dir.
	if _, err := sessA.boundaryPolicy().Check(target, tools.AccessReadWrite); err != nil {
		t.Fatalf("connection A should be allowed to write under its allow-dir, got: %v", err)
	}
	// B must NOT — the grant must not have leaked across connections.
	if _, err := sessB.boundaryPolicy().Check(target, tools.AccessReadWrite); err == nil {
		t.Fatalf("connection B must be refused under A's allow-dir — grant leaked across connections")
	}

	// Sanity: both still hold the workspace baseline read-write.
	wsFile := filepath.Join(ws, "main.go")
	for name, s := range map[string]*connSession{"A": sessA, "B": sessB} {
		if _, err := s.boundaryPolicy().Check(wsFile, tools.AccessReadWrite); err != nil {
			t.Fatalf("connection %s lost its workspace baseline: %v", name, err)
		}
	}
}

// TestAllowDir_PreservedAcrossRepin checks the client's grant survives a
// workspace re-pin (a client reusing one serve across chats), matching the
// behaviour that the grant is bound to the connection, not the workspace.
func TestAllowDir_PreservedAcrossRepin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()

	rootA := freshTempDir(t)
	rootB := freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	allowDir := freshTempDir(t)
	target := filepath.Join(allowDir, "file.txt")

	s := newConnSession(context.Background(), pool, nil, store, nil, newSharedBudgets())
	defer s.close()
	s.onAllowDirs([]string{allowDir})
	s.attachWorkspace(context.Background(), "file://"+rootA)

	if _, err := s.repinWorkspace(context.Background(), rootB, ""); err != nil {
		t.Fatalf("repin: %v", err)
	}

	if _, err := s.boundaryPolicy().Check(target, tools.AccessReadWrite); err != nil {
		t.Fatalf("allow-dir grant should persist across re-pin, got: %v", err)
	}
}
