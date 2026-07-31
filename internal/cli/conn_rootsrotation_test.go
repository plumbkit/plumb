package cli

// conn_rootsrotation_test.go — issue #182's roots-rotation path. A client that
// reports MULTIPLE roots and reorders them must not drag the connection's pin
// between projects: rootFromRoots takes Roots[0] only, and before this fix
// onRootsChanged re-pinned to it on every notification, so a mere reorder
// drifted the pin with no session_start in between. The rule: keep the pinned
// root while it is still reported; re-pin only when it is actually removed.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
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

func TestRootsChanged_LogsReceivedRootsBounded(t *testing.T) {
	// The roots-changed log must record what the client actually reported — the
	// pin-drift evidence issue #182's grep needed — bounded so a pathological
	// multi-root client cannot flood the log line.
	store, ss := newOriginStore(t)
	rootA := freshTempDir(t)
	mustGitDir(t, rootA)

	extras := []string{"b", "c", "d", "e", "f", "g", "h", "i", "j"}
	uris := make([]string, 0, len(extras)+1)
	uris = append(uris, "file://"+rootA)
	for _, extra := range extras {
		uris = append(uris, "file:///nowhere/"+extra)
	}
	quoted := make([]string, 0, len(uris))
	for _, u := range uris {
		quoted = append(quoted, `{"uri":"`+u+`"}`)
	}
	fake := mcp.RequestFn(func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		if method != "roots/list" {
			return nil, nil
		}
		return json.RawMessage(`{"roots":[` + strings.Join(quoted, ",") + `]}`), nil
	})

	s := newPersistSession(t, store, ss, "proxyX")
	buf := captureLog(s)
	s.handleRootsListChanged(context.Background(), fake)

	for _, want := range []string{"roots changed", "count=10", "file://" + rootA, "+2 more"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("roots-changed log missing %q:\n%s", want, buf.String())
		}
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
