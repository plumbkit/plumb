package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/session"
)

// mustGitDir makes dir a git-rooted project so workspacePool.Detect resolves it
// with LanguageNone — no real LSP binary is needed for the re-pin tests.
func mustGitDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// sessionFolder reads the persisted Folder for the session with the given ID.
func sessionFolder(t *testing.T, id string) string {
	t.Helper()
	infos, err := session.List()
	if err != nil {
		t.Fatalf("session.List: %v", err)
	}
	for _, info := range infos {
		if info.ID == id {
			return info.Folder
		}
	}
	t.Fatalf("session %s not found", id)
	return ""
}

// TestRepinWorkspace_SwitchesPinnedRoot is the regression test for the
// permanently-welded-connection bug: a connection pinned to project A must be
// able to switch to project B via an explicit re-pin, instead of staying locked
// to the first project it ever touched (the Claude Desktop reused-connection
// case). Verifies both the live acquiredRoot and the persisted session file.
//
// The workspace dirs use freshTempDir (not t.TempDir): the plumb repo sets
// GOTMPDIR=.testcache, so t.TempDir lands inside the source tree and Detect
// walks up to plumb's own go.mod. freshTempDir lands under /var/folders|/tmp,
// where the .git markers below are the nearest root (language "none", no LSP).
func TestRepinWorkspace_SwitchesPinnedRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()

	rootA := freshTempDir(t)
	rootB := freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newConnSession(context.Background(), pool, nil, store, nil, &sync.Map{})
	defer s.close()

	s.attachWorkspace(context.Background(), "file://"+rootA)
	if got := s.workspace(); got != rootA {
		t.Fatalf("attach: workspace = %s, want %s", got, rootA)
	}

	newRoot, err := s.repinWorkspace(context.Background(), rootB)
	if err != nil {
		t.Fatalf("repin: %v", err)
	}
	if newRoot != rootB || s.workspace() != rootB {
		t.Fatalf("repin: workspace = %s (returned %s), want %s", s.workspace(), newRoot, rootB)
	}
	if folder := sessionFolder(t, s.sessID); folder != rootB {
		t.Fatalf("session file Folder = %s, want %s", folder, rootB)
	}

	// Re-pinning to the already-pinned root is a no-op (no error, same root).
	again, err := s.repinWorkspace(context.Background(), rootB)
	if err != nil || again != rootB {
		t.Fatalf("no-op repin: returned %s, err %v; want %s, nil", again, err, rootB)
	}
}

// TestRepinWorkspace_MarkerlessFolderBecomesWorkspace covers the user's stated
// detection model: when an explicit folder has no .plumb/marker/.git anywhere
// above it, the folder itself becomes the workspace (via SynthesiseRoot) rather
// than the re-pin failing. freshTempDir is required so the folder is genuinely
// markerless (a t.TempDir would resolve to plumb's go.mod — see above).
func TestRepinWorkspace_MarkerlessFolderBecomesWorkspace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()

	rootA := freshTempDir(t)
	mustGitDir(t, rootA)
	bare := freshTempDir(t) // no .git, no language marker, no go.mod ancestor

	s := newConnSession(context.Background(), pool, nil, store, nil, &sync.Map{})
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+rootA)

	newRoot, err := s.repinWorkspace(context.Background(), bare)
	if err != nil {
		t.Fatalf("repin to marker-less dir: %v", err)
	}
	if newRoot != bare || s.workspace() != bare {
		t.Fatalf("marker-less repin: workspace = %s (returned %s), want %s", s.workspace(), newRoot, bare)
	}
}
