package cli

// conn_rootsrotation_test.go — issue #182's roots-rotation path. A client that
// reports MULTIPLE roots and reorders them must not drag the connection's pin
// between projects: rootFromRoots takes Roots[0] only, and before this fix
// onRootsChanged re-pinned to it on every notification, so a mere reorder
// drifted the pin with no session_start in between. The rule: keep the pinned
// root while it is still reported; re-pin only when it is actually removed.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRootsRotation_ReorderKeepsCurrentPin(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA) // client roots [A, B] → pin A
	if got := s.workspace(); got != rootA {
		t.Fatalf("setup: workspace = %q, want %q", got, rootA)
	}

	// Client reorders to [B, A] and fires roots/list_changed. A is still reported —
	// only the order changed — so the pin must stay on A.
	s.onRootsChanged(context.Background(), []string{"file://" + rootB, "file://" + rootA})
	if got := s.workspace(); got != rootA {
		t.Fatalf("a roots reorder drifted the pin: got %q, want %q (A still reported)", got, rootA)
	}
}

func TestRootsChanged_RepinWhenPinnedRootRemoved(t *testing.T) {
	// The genuine case: the client's roots change so the pinned root is GONE. That
	// is a real workspace switch, so the connection re-pins to the first reported.
	store, ss := newOriginStore(t)
	rootA, rootB, rootC := freshTempDir(t), freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	mustGitDir(t, rootC)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)

	s.onRootsChanged(context.Background(), []string{"file://" + rootB, "file://" + rootC})
	if got := s.workspace(); got != rootB {
		t.Fatalf("pinned root removed but did not re-pin: got %q, want %q", got, rootB)
	}
}

func TestRootsChanged_SingleRootSwitchStillRepins(t *testing.T) {
	// A single-root client that switches its one root [A]->[B] must still follow —
	// A is gone, so this is a real switch, not a reorder.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)

	s.onRootsChanged(context.Background(), []string{"file://" + rootB})
	if got := s.workspace(); got != rootB {
		t.Fatalf("single-root switch did not re-pin: got %q, want %q", got, rootB)
	}
}

func TestRootsChanged_SubfolderOfPinnedRootKeepsPin(t *testing.T) {
	// The client re-reports a SUBFOLDER of the pinned root (which Detects back up
	// to the same root). That still counts as "our root is reported" — keep the pin.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	sub := filepath.Join(rootA, "pkg")
	mustMkdir(t, sub)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)

	s.onRootsChanged(context.Background(), []string{"file://" + rootB, "file://" + sub})
	if got := s.workspace(); got != rootA {
		t.Fatalf("a subfolder of the pinned root drifted the pin: got %q, want %q", got, rootA)
	}
}

func TestRootsChanged_FirstAttachPinsFirstRoot(t *testing.T) {
	// On a not-yet-attached connection, the notification attaches the first root
	// (unchanged behaviour).
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	s.onRootsChanged(context.Background(), []string{"file://" + rootA, "file://" + rootB})
	if got := s.workspace(); got != rootA {
		t.Fatalf("first attach = %q, want %q", got, rootA)
	}
}

func TestRootsChanged_EmptyKeepsPin(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA := freshTempDir(t)
	mustGitDir(t, rootA)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)

	s.onRootsChanged(context.Background(), nil)
	if got := s.workspace(); got != rootA {
		t.Fatalf("empty roots change should keep the pin: got %q, want %q", got, rootA)
	}
}
