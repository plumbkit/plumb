package cli

// conn_pinorigin_test.go — the pin-origin discriminator (sessionstate.PinSource).
//
// A persisted pin records WHY it was pinned, because a reconnecting connection
// must tell a workspace the caller chose (session_start) from a stale copy of
// wherever the client happened to launch (roots). Only the former outranks a
// fresh roots/list answer. See conn_attach_hint.go for the full ladder.

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// newOriginStore opens a real session-state store under a temp data dir.
func newOriginStore(t *testing.T) (*config.Store, *sessionstate.Store) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	return config.NewStore(config.Defaults()), ss
}

func TestPersistPin_RootsOriginRecorded(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+root) // the OnInit roots rung

	_, _, src, ok, err := ss.LoadPin("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadPin: ok=%v err=%v", ok, err)
	}
	if src != sessionstate.PinSourceRoots {
		t.Fatalf("source = %q, want %q — a client root is not a deliberate pin", src, sessionstate.PinSourceRoots)
	}
}

func TestPersistPin_SessionStartOriginRecorded(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	if _, err := s.repinWorkspace(context.Background(), root, ""); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}

	_, _, src, ok, err := ss.LoadPin("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadPin: ok=%v err=%v", ok, err)
	}
	if src != sessionstate.PinSourceSessionStart {
		t.Fatalf("source = %q, want %q", src, sessionstate.PinSourceSessionStart)
	}
}

func TestPersistPin_RootsChangedIsNotASessionStartPin(t *testing.T) {
	// onRootsChanged shares repinWorkspace's machinery but is the client moving
	// its folder, not the caller naming a workspace. Recording it as session_start
	// would let it outrank a later roots answer on reconnect.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)
	s.onRootsChanged(context.Background(), "file://"+rootB)

	ws, _, src, ok, err := ss.LoadPin("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadPin: ok=%v err=%v", ok, err)
	}
	if ws != rootB {
		t.Fatalf("roots change did not re-pin: got %q, want %q", ws, rootB)
	}
	if src != sessionstate.PinSourceRoots {
		t.Fatalf("source = %q, want %q", src, sessionstate.PinSourceRoots)
	}
}

func TestPersistPin_SameRootSessionStartPromotesRootsOrigin(t *testing.T) {
	// A session_start(workspace=B) for a B already attached from client roots must
	// UPGRADE the stored origin roots→session_start. attachOrRepinTo returns early
	// when the root and language are unchanged, so without a promotion step the
	// deliberate intent is never recorded, and a later restart whose client roots
	// point elsewhere beats the pin. The persisted-pin channel must be correct on
	// its own, independent of the proxy replay.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+root) // client roots → origin roots
	if _, _, src, _, _ := ss.LoadPin("proxyX"); src != sessionstate.PinSourceRoots {
		t.Fatalf("precondition: source = %q, want roots", src)
	}

	if _, err := s.repinWorkspace(context.Background(), root, ""); err != nil { // explicit, same root
		t.Fatalf("repinWorkspace: %v", err)
	}

	_, _, src, ok, err := ss.LoadPin("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadPin: ok=%v err=%v", ok, err)
	}
	if src != sessionstate.PinSourceSessionStart {
		t.Fatalf("same-root session_start did not promote the origin: got %q, want %q",
			src, sessionstate.PinSourceSessionStart)
	}
}

func TestPersistPin_SameRootRootsChangeDoesNotDemote(t *testing.T) {
	// The other direction of the same edge: a spurious onRootsChanged for the
	// already-attached root must NOT demote a deliberate session_start pin back to
	// roots. Promotion is one-way.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	if _, err := s.repinWorkspace(context.Background(), root, ""); err != nil { // origin session_start
		t.Fatalf("repinWorkspace: %v", err)
	}
	s.onRootsChanged(context.Background(), "file://"+root) // same root, roots origin

	_, _, src, ok, err := ss.LoadPin("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadPin: ok=%v err=%v", ok, err)
	}
	if src != sessionstate.PinSourceSessionStart {
		t.Fatalf("a same-root roots notification demoted the pin: got %q, want %q",
			src, sessionstate.PinSourceSessionStart)
	}
}

func TestPersistPin_IncidentalToolPathNotPersisted(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspacePin(context.Background(), "file://"+root, sessionstate.PinSourceUnknown)

	if _, _, _, ok, _ := ss.LoadPin("proxyX"); ok {
		t.Fatal("an incidental auto-attach must never become the sticky persisted pin")
	}
}

func TestRehydratePin_PreservesSessionStartSource(t *testing.T) {
	// The downgrade guard. rehydratePin re-attaches, and that attach re-persists
	// the pin. If it stamped PinSourceRoots over a session_start row, the pin would
	// silently stop outranking client roots and lose the very next reconnect.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	before := newPersistSession(t, store, ss, "proxyX")
	if _, err := before.repinWorkspace(context.Background(), root, ""); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != root {
		t.Fatalf("pin did not survive reconnect: got %q, want %q", got, root)
	}

	_, _, src, ok, err := ss.LoadPin("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadPin: ok=%v err=%v", ok, err)
	}
	if src != sessionstate.PinSourceSessionStart {
		t.Fatalf("rehydrate downgraded the pin to %q, want %q", src, sessionstate.PinSourceSessionStart)
	}
}

func TestRehydratePin_LegacyRowAttachesWithoutRewriting(t *testing.T) {
	// A row written before the discriminator existed still attaches, but must not
	// be rewritten with an invented origin — its unknown source is what keeps the
	// upgrade behaviour-neutral until the next deliberate re-pin.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	if err := ss.UpsertPin("proxyX", root, LanguageNone, sessionstate.PinSourceUnknown); err != nil {
		t.Fatalf("seed legacy pin: %v", err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	s.rehydratePin(context.Background())
	if got := s.workspace(); got != root {
		t.Fatalf("legacy pin did not attach: got %q, want %q", got, root)
	}
	_, _, src, _, _ := ss.LoadPin("proxyX")
	if src != sessionstate.PinSourceUnknown {
		t.Fatalf("legacy row was rewritten as %q, want it left unknown", src)
	}
}
