package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
)

// mustGitDir makes dir a git-rooted project so workspacePool.Detect resolves it
// with LanguageNone — no real LSP binary is needed for the re-pin tests.
func mustGitDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestRepinWorkspace_IgnoresInactiveLanguageOverride verifies the language
// override is a no-op when the named language is not active (uninstalled,
// disabled, or unknown): detection wins, so a typo or an absent server never
// breaks the pin or attaches the wrong primary. detectTestPool has only go +
// python, so "swift" is inactive; the git-rooted dir resolves as LanguageNone.
func TestRepinWorkspace_IgnoresInactiveLanguageOverride(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newConnSession(context.Background(), pool, nil, store, nil, newSharedBudgets())
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+root)

	if _, err := s.repinWorkspace(context.Background(), root, "swift"); err != nil {
		t.Fatalf("repin: %v", err)
	}
	if got := s.view().acquiredLanguage; got != LanguageNone {
		t.Errorf("acquiredLanguage = %q, want %q (an inactive language override must be ignored)", got, LanguageNone)
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

	s := newConnSession(context.Background(), pool, nil, store, nil, newSharedBudgets())
	defer s.close()

	s.attachWorkspace(context.Background(), "file://"+rootA)
	if got := s.workspace(); got != rootA {
		t.Fatalf("attach: workspace = %s, want %s", got, rootA)
	}

	newRoot, err := s.repinWorkspace(context.Background(), rootB, "")
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
	again, err := s.repinWorkspace(context.Background(), rootB, "")
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

	s := newConnSession(context.Background(), pool, nil, store, nil, newSharedBudgets())
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+rootA)

	newRoot, err := s.repinWorkspace(context.Background(), bare, "")
	if err != nil {
		t.Fatalf("repin to marker-less dir: %v", err)
	}
	if newRoot != bare || s.workspace() != bare {
		t.Fatalf("marker-less repin: workspace = %s (returned %s), want %s", s.workspace(), newRoot, bare)
	}
}

// TestRepinWorkspace_ResetsTrackers verifies a re-pin clears the per-session
// read/write tracking: paths plumb touched in project A must not carry over to
// project B, where plumb has written and read nothing yet (so B's dirty-guard
// and strict-mode read check start clean).
func TestRepinWorkspace_ResetsTrackers(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()

	rootA := freshTempDir(t)
	rootB := freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newConnSession(context.Background(), pool, nil, store, nil, newSharedBudgets())
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+rootA)

	writtenA := filepath.Join(rootA, "touched.go")
	readA := filepath.Join(rootA, "seen.go")
	s.writeTracker.Record(writtenA)
	s.readTracker.Record(readA, time.Now(), "")
	if !s.writeTracker.Wrote(writtenA) || s.readTracker.Mtime(readA).IsZero() {
		t.Fatal("precondition: tracker should hold the recorded paths before re-pin")
	}

	if _, err := s.repinWorkspace(context.Background(), rootB, ""); err != nil {
		t.Fatalf("repin: %v", err)
	}
	if s.writeTracker.Wrote(writtenA) {
		t.Error("write tracker should be cleared on re-pin")
	}
	if !s.readTracker.Mtime(readA).IsZero() {
		t.Error("read tracker should be cleared on re-pin")
	}
}

// TestOnRootsChanged_RepinsOnChange covers the roots/list_changed path: the
// first reported root pins the connection, a later *different* root re-pins it
// (an editor switching folders), and an empty root leaves the current pin
// untouched rather than tearing the workspace down.
func TestOnRootsChanged_RepinsOnChange(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()

	rootA := freshTempDir(t)
	rootB := freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newConnSession(context.Background(), pool, nil, store, nil, newSharedBudgets())
	defer s.close()

	// First report pins A (connection not yet attached).
	s.onRootsChanged(context.Background(), "file://"+rootA)
	if got := s.workspace(); got != rootA {
		t.Fatalf("first roots change: workspace = %s, want %s", got, rootA)
	}

	// A genuinely different root re-pins to B.
	s.onRootsChanged(context.Background(), "file://"+rootB)
	if got := s.workspace(); got != rootB {
		t.Fatalf("changed roots: workspace = %s, want %s", got, rootB)
	}

	// An empty root (client cannot satisfy roots/list) keeps the current pin.
	s.onRootsChanged(context.Background(), "")
	if got := s.workspace(); got != rootB {
		t.Fatalf("empty roots change should keep pin: workspace = %s, want %s", got, rootB)
	}
}
