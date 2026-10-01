package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// TestPersist_SpelledReadRowRehydratesUnderEverySpelling: a daemon predating
// issue #524 persisted a read under the path as the agent spelled it. After a
// restart that row must answer for the file under any spelling, or strict mode
// refuses an edit through the canonical path ("has not been read") and the
// default staleness guard fails open over a peer's change.
func TestPersist_SpelledReadRowRehydratesUnderEverySpelling(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(root, "real", "a.go")
	if err := os.WriteFile(realPath, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_700_000_000, 444)

	// An older daemon's row: keyed by the alias spelling, written straight to
	// the store as that daemon's persist sink would have.
	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspace(context.Background(), "file://"+root)
	ws := before.view().acquiredRoot
	if err := ss.UpsertRead("proxyX", ws, filepath.Join(root, "alias", "a.go"), mtime, "sha-a"); err != nil {
		t.Fatal(err)
	}
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	after.attachWorkspace(context.Background(), "file://"+root)
	for _, p := range []string{realPath, filepath.Join(root, "alias", "a.go")} {
		if got := after.readTracker.Mtime(p); !got.Equal(mtime) {
			t.Fatalf("rehydrated Mtime(%s) = %v, want %v — a spelled row must answer for every spelling", p, got, mtime)
		}
	}
}
