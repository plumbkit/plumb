package cli

// conn_workspacehint_test.go — the serve-proxy cwd workspace hint: storage,
// Detect-validated attach, precedence (first-wins below roots and the
// persisted pin), and the never-persisted guarantee.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// newHintSession builds a bare connSession (no persistence store) for the
// hint tests that do not exercise the persisted pin.
func newHintSession(t *testing.T) *connSession {
	t.Helper()
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	return s
}

func TestOnWorkspaceHintStores(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newHintSession(t)

	s.onWorkspaceHint("/Users/dev/proj")
	if got := s.view().workspaceHint; got != "/Users/dev/proj" {
		t.Fatalf("workspaceHint = %q, want /Users/dev/proj", got)
	}
	// An empty hint is a no-op — it must not clear a stored one.
	s.onWorkspaceHint("")
	if got := s.view().workspaceHint; got != "/Users/dev/proj" {
		t.Fatalf("empty hint cleared the stored one: %q", got)
	}
}

func TestAttachFromHint_AttachesDetectedRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newHintSession(t)
	s.onWorkspaceHint(root)
	s.attachFromHint(context.Background())
	if got := s.workspace(); got != root {
		t.Fatalf("attachFromHint pinned %q, want %q", got, root)
	}
}

func TestAttachFromHint_DetectFailureLeavesUnattached(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// A bare temp dir under the OS TMPDIR: no marker, no .git, and nothing
	// resolvable above it — pool.Detect must fail and the hint attach nothing.
	dir := freshTempDir(t)

	s := newHintSession(t)
	s.onWorkspaceHint(dir)
	s.attachFromHint(context.Background())
	if got := s.workspace(); got != "" {
		t.Fatalf("a hint with no project boundary attached %q; must stay unattached", got)
	}
}

func TestAttachFromHint_FirstWins(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rootA := freshTempDir(t)
	rootB := freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newHintSession(t)
	s.attachWorkspace(context.Background(), "file://"+rootA)
	s.onWorkspaceHint(rootB)
	s.attachFromHint(context.Background())
	if got := s.workspace(); got != rootA {
		t.Fatalf("hint overrode an already-attached workspace: got %q, want %q", got, rootA)
	}
}

// TestAttachFromHint_NotPersistedAsPin: a hint attach must never become the
// sticky persisted pin — a reconnect restores only what the caller explicitly
// chose, so the replayed hint cannot mask a deliberate re-pin elsewhere.
func TestAttachFromHint_NotPersistedAsPin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	s.onWorkspaceHint(root)
	s.attachFromHint(context.Background())
	if got := s.workspace(); got != root {
		t.Fatalf("hint attach pinned %q in-memory, want %q", got, root)
	}

	if _, _, ok, err := ss.LoadPin("proxyX"); err != nil || ok {
		t.Fatalf("hint attach persisted a pin (ok=%v err=%v); only explicit pins are sticky", ok, err)
	}
}

func TestRootFromClient_FallsBackToHint(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := freshTempDir(t)
	mustGitDir(t, root)

	// No client request channel and no hint ⇒ nothing to resolve.
	s := newHintSession(t)
	if got := s.rootFromClient(context.Background()); got != "" {
		t.Fatalf("no roots, no hint: rootFromClient = %q, want \"\"", got)
	}

	// No client request channel but a Detect-resolvable hint ⇒ the hint's root.
	s.onWorkspaceHint(root)
	if got := s.rootFromClient(context.Background()); got != root {
		t.Fatalf("rootFromClient = %q, want the hint root %q", got, root)
	}

	// A hint that resolves to no project boundary yields "" — never a raw path.
	s2 := newHintSession(t)
	s2.onWorkspaceHint(freshTempDir(t))
	if got := s2.rootFromClient(context.Background()); got != "" {
		t.Fatalf("an unresolvable hint leaked through rootFromClient: %q", got)
	}
}

func TestRootFromClient_RootsBeatHint(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rootA := freshTempDir(t)
	rootB := freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newHintSession(t)
	s.onWorkspaceHint(rootB)
	s.setClientRequest(func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		if method != "roots/list" {
			t.Fatalf("unexpected client request %q", method)
		}
		return json.RawMessage(`{"roots":[{"uri":"file://` + rootA + `"}]}`), nil
	})
	if got := s.rootFromClient(context.Background()); got != rootA {
		t.Fatalf("rootFromClient = %q; client roots must beat the hint (%q)", got, rootA)
	}
}

// TestOnInitOrder_PersistedPinBeatsHint mirrors the OnInit rungs: with a
// persisted explicit pin on A and a replayed hint pointing at B, the reconnect
// must land on A — the pin records a deliberate choice; the hint is only the
// proxy's launch directory.
func TestOnInitOrder_PersistedPinBeatsHint(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	rootA := freshTempDir(t)
	rootB := freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspacePin(context.Background(), "file://"+rootA, true) // explicit
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	after.onWorkspaceHint(rootB)
	// The OnInit fallback ladder for a client that reports no roots:
	after.rehydratePin(context.Background())
	if after.workspace() == "" {
		after.attachFromHint(context.Background())
	}
	if got := after.workspace(); got != rootA {
		t.Fatalf("reconnect pinned %q; the persisted explicit pin %q must beat the hint", got, rootA)
	}
}
