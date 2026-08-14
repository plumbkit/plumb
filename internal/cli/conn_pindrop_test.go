package cli

// conn_pindrop_test.go — drop-don't-widen at the restore path.
//
// A persisted or replayed pin is restored under pinTriggerRestore, which the
// sticky-pin guard deliberately does not gate — so nothing else stands between
// a stale pin and the detect walk's upward climb. A pinned worktree that was
// deleted rehydrated to the ENCLOSING REPOSITORY: a write surface strictly
// wider than the one the caller chose, reached silently, with the healthy-looking
// "workspace rehydrated" log line. These tests pin the contract: a root that no
// longer verifies is DROPPED — logged, deleted from the store, left unattached
// for the lower ladder rungs — and never attached at an ancestor.
//
// Each test was mutation-checked: reverting restoreRootIntact's verification
// makes it fail.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// pinWorktree builds parent (a git repo) containing sub (its own git root, as
// a git worktree would be), pins sub explicitly so the pin persists, and
// returns the reconnected session ready to rehydrate.
func pinWorktree(t *testing.T) (parent, sub string) {
	t.Helper()
	parent = freshTempDir(t)
	mustGitDir(t, parent)
	sub = filepath.Join(parent, "wt")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitDir(t, sub)
	return parent, sub
}

// TestRehydratePin_DeletedRootDoesNotWidenToAncestor is the headline bug: the
// pinned worktree is deleted, and the restore must NOT come back pinned to the
// enclosing repository.
func TestRehydratePin_DeletedRootDoesNotWidenToAncestor(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, sub := pinWorktree(t)

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspacePin(context.Background(), "file://"+sub, sessionstate.PinSourceSessionStart)
	before.close()

	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}

	after := newPersistSession(t, store, ss, "proxyX")
	after.rehydratePin(context.Background())
	if got := after.workspace(); got == parent {
		t.Fatalf("FAIL-OPEN: deleted worktree rehydrated to the enclosing repo %q", got)
	}
	if got := after.workspace(); got != "" {
		t.Fatalf("dropped pin left the session attached to %q, want honestly unattached", got)
	}
	if _, _, _, ok, err := ss.LoadPin("proxyX"); err != nil || ok {
		t.Fatalf("dropped pin row still stored (ok=%v err=%v); it would re-widen on every reconnect", ok, err)
	}
}

// TestRehydratePin_MarkersRemovedDoesNotWiden: the directory survives but its
// project markers are gone, so Detect now climbs to the parent repo. Same drop.
func TestRehydratePin_MarkersRemovedDoesNotWiden(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, sub := pinWorktree(t)

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspacePin(context.Background(), "file://"+sub, sessionstate.PinSourceSessionStart)
	before.close()

	if err := os.RemoveAll(filepath.Join(sub, ".git")); err != nil {
		t.Fatal(err)
	}

	after := newPersistSession(t, store, ss, "proxyX")
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != "" {
		t.Fatalf("pin whose markers were removed attached %q (parent %q); want a drop", got, parent)
	}
	if _, _, _, ok, _ := ss.LoadPin("proxyX"); ok {
		t.Fatal("dropped pin row still stored")
	}
}

// TestRehydratePin_AliasSpelledPinDropped: a pin row spelled through a symlink
// resolves canonically to a DIFFERENT string. It must be dropped — never
// attached under either spelling, where it would shadow the canonical pin.
func TestRehydratePin_AliasSpelledPinDropped(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	alias := filepath.Join(filepath.Dir(root), "plumb-pin-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })

	if err := ss.UpsertPin("proxyX", alias, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("seed alias pin: %v", err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	s.rehydratePin(context.Background())
	if got := s.workspace(); got != "" {
		t.Fatalf("alias-spelled pin attached %q; want a drop, never a shadow pin", got)
	}
	if _, _, _, ok, _ := ss.LoadPin("proxyX"); ok {
		t.Fatal("alias pin row still stored")
	}
}

// TestRehydratePin_SyntheticRootDeletedLeavesNoGhost: a markerless pin whose
// directory is deleted must not rehydrate at all — the old path re-synthesised
// the same string and pinned a directory that does not exist.
func TestRehydratePin_SyntheticRootDeletedLeavesNoGhost(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t) // no .git here or above: a synthetic (markerless) root

	before := newPersistSession(t, store, ss, "proxyX")
	if _, err := before.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	after := newPersistSession(t, store, ss, "proxyX")
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != "" {
		t.Fatalf("deleted synthetic root rehydrated a ghost pin %q", got)
	}
	if _, _, _, ok, _ := ss.LoadPin("proxyX"); ok {
		t.Fatal("dropped pin row still stored")
	}
}

// TestRehydratePin_SyntheticRootDoesNotClimbToNewGit: a markerless pin whose
// PARENT gains a .git after the pin was taken must not restore to that parent
// through SynthesiseRoot's upward walk.
func TestRehydratePin_SyntheticRootDoesNotClimbToNewGit(t *testing.T) {
	store, ss := newOriginStore(t)
	parent := freshTempDir(t)
	root := filepath.Join(parent, "wt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	before := newPersistSession(t, store, ss, "proxyX")
	if _, err := before.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	if got := before.workspace(); got != root {
		t.Fatalf("precondition: synthetic pin = %q, want %q", got, root)
	}
	before.close()

	mustGitDir(t, parent) // the trigger: a project boundary appears ABOVE the pin

	after := newPersistSession(t, store, ss, "proxyX")
	after.rehydratePin(context.Background())
	if got := after.workspace(); got == parent {
		t.Fatalf("FAIL-OPEN: synthetic pin climbed to new .git ancestor %q", got)
	}
	if got := after.workspace(); got != "" {
		t.Fatalf("synthetic pin restored to %q after a .git appeared above it; want a drop", got)
	}
}

// TestAttachOnInit_DroppedPinFallsThroughToRoots: the OnInit ladder's drop is
// not a dead end — with the stale pin gone, the client's own roots still attach.
func TestAttachOnInit_DroppedPinFallsThroughToRoots(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA := freshTempDir(t)
	mustGitDir(t, rootA)
	_, sub := pinWorktree(t)

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspacePin(context.Background(), "file://"+sub, sessionstate.PinSourceSessionStart)
	before.close()

	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}

	calls := 0
	after := reconnect(t, store, ss, rootA, &calls)
	if got := after.workspace(); got != rootA {
		t.Fatalf("after the drop the ladder should land on client roots %q, got %q", rootA, got)
	}
}

// TestToolResultMeta_EchoesCanonicalRoot: the daemon echoes the resolved root
// on a session_start(workspace=…) result so the proxy can commit the canonical
// spelling; anything else gets no _meta.
func TestToolResultMeta_EchoesCanonicalRoot(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspacePin(context.Background(), "file://"+root, sessionstate.PinSourceSessionStart)

	meta := s.toolResultMeta(context.Background(), "session_start", []byte(`{"workspace":"`+root+`"}`))
	got, ok := meta[mcp.MetaResolvedWorkspaceKey]
	if !ok || got != root {
		t.Fatalf("session_start result _meta = %v, want %s=%q", meta, mcp.MetaResolvedWorkspaceKey, root)
	}
	if m := s.toolResultMeta(context.Background(), "session_start", []byte(`{}`)); m != nil {
		t.Fatalf("workspace-less session_start got _meta %v", m)
	}
	if m := s.toolResultMeta(context.Background(), "read_file", []byte(`{"workspace":"`+root+`"}`)); m != nil {
		t.Fatalf("a non-session_start tool got _meta %v", m)
	}
}
