package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// newPersistSession builds a connSession wired to a shared session-state store
// with the given proxy session ID set (as the initialize handshake would), then
// returns it. The caller attaches a workspace next.
func newPersistSession(t *testing.T, store *config.Store, ss *sessionstate.Store, proxyID string) *connSession {
	t.Helper()
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, ss, newSharedBudgets())
	t.Cleanup(s.close)
	s.onProxySession(proxyID) // fires before attach, mirroring handleInitialize
	return s
}

// TestPersist_ReadTrackingSurvivesRestart is the headline test: a read recorded
// under proxy session X for workspace W is rehydrated by a *fresh* connSession
// (a daemon restart) that reconnects under the same X and re-attaches W, so a
// strict-mode edit no longer fails with "not read this session".
func TestPersist_ReadTrackingSurvivesRestart(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)
	path := filepath.Join(root, "a.go")
	mtime := time.Unix(1_700_000_000, 444)

	// --- before restart: record a read under proxy X ---
	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspace(context.Background(), "file://"+root)
	before.readTracker.Record(path, mtime, "sha-a") // fires the persist sink
	before.close()

	// --- after restart: a fresh session, same proxy X, same store ---
	after := newPersistSession(t, store, ss, "proxyX")
	after.attachWorkspace(context.Background(), "file://"+root)

	if got := after.readTracker.Mtime(path); !got.Equal(mtime) {
		t.Fatalf("rehydrated mtime = %v, want %v — read-tracking did not survive the restart", got, mtime)
	}
}

func TestPersist_PerSessionIsolation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)
	path := filepath.Join(root, "a.go")

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspace(context.Background(), "file://"+root)
	before.readTracker.Record(path, time.Unix(1, 0), "")
	before.close()

	// A different proxy ID must NOT inherit proxyX's reads.
	other := newPersistSession(t, store, ss, "proxyY")
	other.attachWorkspace(context.Background(), "file://"+root)
	if got := other.readTracker.Mtime(path); !got.IsZero() {
		t.Fatalf("proxyY rehydrated proxyX's read (mtime %v); sessions must be isolated", got)
	}
}

func TestPersist_RepinDoesNotResurrectOtherWorkspace(t *testing.T) {
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
	pathA := filepath.Join(rootA, "a.go")

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspace(context.Background(), "file://"+rootA)
	before.readTracker.Record(pathA, time.Unix(1, 0), "")
	before.close()

	// Reconnect under the same proxy but attach a DIFFERENT workspace: rootA's
	// reads must not leak into rootB (the reset-on-repin invariant).
	after := newPersistSession(t, store, ss, "proxyX")
	after.attachWorkspace(context.Background(), "file://"+rootB)
	if got := after.readTracker.Mtime(pathA); !got.IsZero() {
		t.Fatalf("rootA's read leaked into rootB (mtime %v)", got)
	}
}

func TestPersist_PinRehydratesUnpinnedConnection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)

	// First connection pins the workspace, persisting the pin under proxyX.
	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspace(context.Background(), "file://"+root)
	before.close()

	// A reconnected connection that reports NO roots (no attachWorkspace) must
	// come back pinned via the persisted pin.
	after := newPersistSession(t, store, ss, "proxyX")
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != root {
		t.Fatalf("rehydratePin pinned %q, want %q", got, root)
	}
}

func TestPersist_DisabledWritesNothing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg := config.Defaults()
	cfg.Session.PersistState = false
	store := config.NewStore(cfg)
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)
	path := filepath.Join(root, "a.go")

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspace(context.Background(), "file://"+root)
	before.readTracker.Record(path, time.Unix(1, 0), "")
	before.close()

	recs, err := ss.LoadReads("proxyX", root)
	if err != nil {
		t.Fatalf("LoadReads: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("persist_state=false wrote %d rows, want 0", len(recs))
	}
}

func TestPersist_NoProxyIDWritesNothing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)
	path := filepath.Join(root, "a.go")

	// A non-serve client never injects a proxy ID — nothing should be persisted.
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, ss, newSharedBudgets())
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+root)
	s.readTracker.Record(path, time.Unix(1, 0), "")

	recs, err := ss.LoadReads("", root)
	if err != nil {
		t.Fatalf("LoadReads: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("a connection with no proxy ID persisted %d rows, want 0", len(recs))
	}
}
