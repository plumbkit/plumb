package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/paths"
)

func TestConnSessionRecordHistory(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxy-history")
	s.historyStore = newHistoryStore(nil)
	defer s.historyStore.Close()

	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}

	ctx := context.Background()
	callCtx := mcp.WithCallID(mcp.WithLogicalAgent(ctx, "agentA"), "01CID000000000000000000000")
	s.recordHistory(callCtx, history.Change{
		Op:    history.OpCreate,
		Path:  filepath.Join(root, "a.go"),
		After: history.SideFromBytes([]byte("package main\n")),
		At:    time.Now(),
		Kind:  history.KindFile,
	})

	if err := s.historyStore.store().Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	r, err := history.OpenReadOnlyAt(history.DBPath())
	if err != nil {
		t.Fatalf("OpenReadOnlyAt: %v", err)
	}
	defer r.Close()

	entries, err := r.List(history.Filter{Workspace: root})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.CallID != "01CID000000000000000000000" {
		t.Errorf("CallID = %q, want 01CID000000000000000000000", e.CallID)
	}
	if e.Workspace != paths.Canonical(root) {
		t.Errorf("Workspace = %q, want %q", e.Workspace, paths.Canonical(root))
	}
	if e.SessionID != s.sessionID() {
		t.Errorf("SessionID = %q, want %q", e.SessionID, s.sessionID())
	}
	if e.Path != "a.go" {
		t.Errorf("Path = %q, want a.go", e.Path)
	}

	// 2. Sensitive file (.env) with default globs gives Content == withheld:sensitive
	s.recordHistory(callCtx, history.Change{
		Op:    history.OpCreate,
		Path:  filepath.Join(root, ".env"),
		After: history.SideFromBytes([]byte("SECRET=12345\n")),
		At:    time.Now(),
		Kind:  history.KindFile,
	})
	if err := s.historyStore.store().Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	entries, err = r.List(history.Filter{Workspace: root, File: ".env"})
	if err != nil {
		t.Fatalf("List .env: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries for .env, want 1", len(entries))
	}
	if entries[0].Content != history.ContentSensitive {
		t.Errorf("expected ContentSensitive, got %q", entries[0].Content)
	}

	// 3. Disabled config costs nothing
	enqueuedBefore := s.historyStore.store().Enqueued()
	s.mutate(func(v *sessionView) { v.history.Enabled = false })
	s.recordHistory(callCtx, history.Change{
		Op:    history.OpCreate,
		Path:  filepath.Join(root, "b.go"),
		After: history.SideFromBytes([]byte("package main\n")),
		At:    time.Now(),
		Kind:  history.KindFile,
	})
	enqueuedAfter := s.historyStore.store().Enqueued()
	if enqueuedAfter != enqueuedBefore {
		t.Errorf("enqueued count increased when history disabled: before %d, after %d", enqueuedBefore, enqueuedAfter)
	}
	if enqueuedBefore == 0 {
		t.Errorf("positive control: enqueued count before should be > 0, got %d", enqueuedBefore)
	}
}
