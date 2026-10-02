package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/tools/txlog"
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

func TestTxlogRecoveryRowsAreReverts(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxy-recovery")
	s.historyStore = newHistoryStore(nil)
	defer s.historyStore.Close()

	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}

	sink := s.txlogRecoverySink(root)
	sink(txlog.Restored{
		Path:   filepath.Join(root, "f"),
		Before: history.SideFromBytes([]byte("b\n")),
		After:  []byte("a\n"),
		CallID: "",
	})

	if err := s.historyStore.store().Sync(context.Background()); err != nil {
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
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Op != history.OpRevert {
		t.Errorf("Op = %v, want revert", e.Op)
	}
	if e.Reason != "crash_recovery" {
		t.Errorf("Reason = %q, want crash_recovery", e.Reason)
	}
	if e.Tool != "txlog_recovery" {
		t.Errorf("Tool = %q, want txlog_recovery", e.Tool)
	}
	if !strings.HasPrefix(e.CallID, "recovery-") {
		t.Errorf("CallID = %q, want prefix recovery-", e.CallID)
	}
}

// TestCrashRecoveryAtAttachIsRecorded drives a real orphaned txlog through the
// attach path. Recovery runs inside the attach, before the project config is
// applied, so it must already see [history] enabled from the global config —
// the case the direct-sink test above cannot reach.
func TestCrashRecoveryAtAttachIsRecorded(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	if err := os.MkdirAll(filepath.Join(root, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A transaction snapshots f.txt, half-writes it, and the daemon "crashes":
	// the log is left behind.
	l, err := txlog.Begin(root, "01CRASHCALL0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(target, []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("half-written\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newPersistSession(t, store, ss, "proxy-crash-attach")
	s.daemonStartedAt = time.Now().Add(time.Second) // the log predates this daemon
	s.historyStore = newHistoryStore(nil)
	defer s.historyStore.Close()
	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "orig\n" {
		t.Fatalf("control: recovery did not restore the file, got %q", got)
	}
	hst := s.historyStore.store()
	if hst == nil {
		t.Fatal("no history row was even attempted: the store never opened")
	}
	if err := hst.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := history.OpenReadOnlyAt(history.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	entries, err := r.List(history.Filter{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Op != history.OpRevert || entries[0].Reason != "crash_recovery" ||
		entries[0].CallID != "01CRASHCALL0000000000000000" {
		t.Fatalf("crash recovery at attach recorded %+v; want one crash_recovery revert under the manifest's call id", entries)
	}
}

// A write into another project is classified by THAT project's [history]
// config, not by the config of the project the connection is pinned to.
func TestHistoryUsesTheWrittenProjectsConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	connRoot, other := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, connRoot)
	mustGitDir(t, other)
	if err := os.MkdirAll(filepath.Join(other, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".plumb", "config.toml"),
		[]byte("[history]\nsensitive_globs = [\"*.secret\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newPersistSession(t, store, ss, "proxy-history-project")
	s.historyStore = newHistoryStore(nil)
	defer s.historyStore.Close()
	if _, err := s.repinWorkspace(context.Background(), "file://"+connRoot, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	change := func(path string) history.Change {
		return history.Change{
			Op: history.OpCreate, Kind: history.KindFile, At: time.Now(), Path: path,
			After: history.SideFromBytes([]byte("token=abc\n")),
		}
	}
	ctx := context.Background()
	s.recordHistory(ctx, change(filepath.Join(other, "api.secret")))
	// Positive control: the same name in the connection's own project is not
	// sensitive there (its config never mentions *.secret).
	s.recordHistory(ctx, change(filepath.Join(connRoot, "api.secret")))
	if err := s.historyStore.store().Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := history.OpenReadOnlyAt(history.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	contentIn := func(root string) history.Content {
		es, err := r.List(history.Filter{Workspace: root})
		if err != nil || len(es) != 1 {
			t.Fatalf("entries under %s = %+v, %v", root, es, err)
		}
		return es[0].Content
	}
	if got := contentIn(other); got != history.ContentSensitive {
		t.Errorf("write into the other project stored %q; its own sensitive_globs must apply", got)
	}
	if got := contentIn(connRoot); got != history.ContentDiff {
		t.Errorf("control: the connection project's write stored %q, want a diff", got)
	}
}

func TestAgentConfigSetRecordsConfigToml(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxy-agent-config")
	s.historyStore = newHistoryStore(nil)
	defer s.historyStore.Close()

	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	s.mutate(func(v *sessionView) { v.agentConfigWrites = true })

	ctx := mcp.WithCallID(context.Background(), "01AC000000000000000000000")
	if _, err := s.applyAgentConfig(ctx, map[string]any{"tasks.go.build": "go build ./..."}); err != nil {
		t.Fatalf("applyAgentConfig: %v", err)
	}

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
	if e.Tool != "agent_config" {
		t.Errorf("Tool = %q, want agent_config", e.Tool)
	}
	if e.Op != history.OpCreate {
		t.Errorf("Op = %v, want OpCreate", e.Op)
	}
	if e.Path != ".plumb/config.toml" {
		t.Errorf("Path = %q, want .plumb/config.toml", e.Path)
	}
}
