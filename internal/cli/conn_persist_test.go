package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

func TestWorkspaceArgPresent(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"workspace arg", `{"workspace":"/x"}`, true},
		{"empty workspace", `{"workspace":""}`, false},
		{"incidental file_path", `{"file_path":"/x/a.go"}`, false},
		{"incidental path", `{"path":"/x"}`, false},
		{"no fields", `{}`, false},
		{"invalid json", `not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := workspaceArgPresent(json.RawMessage(c.args)); got != c.want {
				t.Errorf("workspaceArgPresent(%s) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

// TestPersist_AutoAttachDoesNotPersistPin: an auto-attach seeded from an
// incidental tool path must NOT persist the pin, so a reconnect does not
// resurrect a workspace the caller never explicitly chose.
func TestPersist_AutoAttachDoesNotPersistPin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspacePin(context.Background(), "file://"+root, sessionstate.PinSourceUnknown) // auto
	if got := before.workspace(); got != root {
		t.Fatalf("auto-attach pinned %q in-memory, want %q", got, root)
	}
	before.close()

	// A reconnected connection must find NO persisted pin (auto-attach wrote none).
	after := newPersistSession(t, store, ss, "proxyX")
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != "" {
		t.Fatalf("auto-attach persisted a pin (%q); only explicit pins are sticky", got)
	}
}

// TestPersist_ExplicitAttachPersistsPin: an explicit attach (session_start
// workspace arg / client root) persists and survives a reconnect.
func TestPersist_ExplicitAttachPersistsPin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspacePin(context.Background(), "file://"+root, sessionstate.PinSourceSessionStart) // explicit
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != root {
		t.Fatalf("explicit pin did not survive reconnect: got %q, want %q", got, root)
	}
}

// TestOnBeforeTool_IncidentalReadRestoresExplicitPin is the headline pin-drift
// fix: on a reconnected, unpinned connection, reading a file in ANOTHER project
// by absolute path restores the last EXPLICIT pin instead of silently drifting
// to the other project.
func TestOnBeforeTool_IncidentalReadRestoresExplicitPin(t *testing.T) {
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

	// Explicitly pin rootA, persisting it.
	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspacePin(context.Background(), "file://"+rootA, sessionstate.PinSourceSessionStart)
	before.close()

	// Reconnect, then read a rootB file by absolute path BEFORE any explicit pin.
	after := newPersistSession(t, store, ss, "proxyX")
	after.onBeforeTool(context.Background(), "read_file", json.RawMessage(`{"file_path":"`+filepath.Join(rootB, "a.go")+`"}`))
	if got := after.workspace(); got != rootA {
		t.Fatalf("incidental rootB read pinned %q; must restore the explicit pin %q", got, rootA)
	}
}

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

// TestPersist_NameSurvivesReconnect: a reconnected connection (same proxy
// session ID, fresh connSession — a daemon restart) comes back under the SAME
// session name instead of a freshly generated one, so mailbox notes addressed
// to it stay deliverable.
func TestPersist_NameSurvivesReconnect(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	before := newPersistSession(t, store, ss, "proxyX")
	name := before.sessionName()
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	if got := after.sessionName(); got != name {
		t.Fatalf("reconnect renamed the session to %q, want the persisted %q", got, name)
	}
}

// TestPersist_RenamedNameSurvivesReconnect: a rename_session (or an inherited
// name) is persisted, so the reconnect restores the RENAMED name, not the
// originally generated one.
func TestPersist_RenamedNameSurvivesReconnect(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	before := newPersistSession(t, store, ss, "proxyX")
	if _, err := before.renameSession("steady-otter"); err != nil {
		t.Fatalf("renameSession: %v", err)
	}
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	if got := after.sessionName(); got != "steady-otter" {
		t.Fatalf("reconnect restored %q, want the renamed steady-otter", got)
	}
}

// TestPersist_NameNotRestoredOntoOverlappingSession: a proxy reconnect can
// overlap its predecessor (the proxy reconnected, the previous connSession is
// still registered). Restoring the name there would leave TWO live sessions
// answering to it — and leave_note delivery matches on the name string, so the
// message would go to whichever asked first. session.Rename refuses with
// ErrNameTaken; the reconnect must treat that as "try again later", keeping its
// generated name and leaving the persisted one for a non-overlapping attempt.
func TestPersist_NameNotRestoredOntoOverlappingSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	// The predecessor stays LIVE (deliberately not closed).
	before := newPersistSession(t, store, ss, "proxyX")
	if _, err := before.renameSession("steady-otter"); err != nil {
		t.Fatalf("renameSession: %v", err)
	}

	after := newPersistSession(t, store, ss, "proxyX")
	if got := after.sessionName(); got == "steady-otter" {
		t.Error("restored a name still held by a live session — leave_note delivery would be ambiguous")
	}
	// The stored mapping is untouched, so the next (non-overlapping) reconnect
	// still restores it.
	if got, ok, _ := ss.LoadIdentity("proxyX"); !ok || got.Name != "steady-otter" {
		t.Errorf("stored identity = (%+v,%v), want the persisted steady-otter left intact", got, ok)
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
	if _, ok, err := ss.LoadIdentity("proxyX"); err != nil || ok {
		t.Fatalf("persist_state=false recorded a session name: ok=%v err=%v", ok, err)
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

// TestPersist_StoredNameRejectedByValidationIsReplaced is the other half of the
// restore contract. A stored name can become invalid under a NEWER daemon:
// "next" was an ordinary session name until it became the mailbox's reserved
// next-arrival address. Unlike a name that is merely busy, this one will NEVER
// become restorable — so leaving the row in place makes every reconnect fail
// identically and the session come back randomly renamed each time, which is
// precisely the churn this persistence exists to prevent, and it is silent at
// Debug. The restore must overwrite an unusable name so it converges.
func TestPersist_StoredNameRejectedByValidationIsReplaced(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	// A row written by an older daemon, before "next" was reserved.
	if err := ss.SaveIdentity("proxyX", "next", ""); err != nil {
		t.Fatalf("SaveName: %v", err)
	}

	first := newPersistSession(t, store, ss, "proxyX")
	name := first.sessionName()
	if name == "next" {
		t.Fatal("restored the reserved name onto a live session")
	}
	first.close()

	stored, ok, err := ss.LoadIdentity("proxyX")
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if !ok || stored.Name != name {
		t.Fatalf("stored identity = (%+v, %v), want the unusable row replaced with %q", stored, ok, name)
	}

	// The point of replacing it: the NEXT reconnect is stable.
	second := newPersistSession(t, store, ss, "proxyX")
	if got := second.sessionName(); got != name {
		t.Fatalf("second reconnect came back as %q, want the now-stable %q", got, name)
	}
}

// TestUnregisteredSession_HasNoMailboxAddress. When session.Register fails the
// connection still serves, and it still needs a display name for the TUI, logs
// and daemon_info. But that fallback name was drawn WITHOUT a uniqueness check,
// and the session has no file for any peer's check to find, so it can silently
// duplicate a live session's name. Messages are claimed exactly once, so an
// addressable shadow would swallow the real recipient's mail — the exact
// failure the uniqueness work exists to close. No registration, no address.
func TestUnregisteredSession_HasNoMailboxAddress(t *testing.T) {
	// XDG_DATA_HOME pointing at a regular file makes Register's MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("seeding the blocker file: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", blocker)

	s := newConnSession(context.Background(), detectTestPool(), nil,
		config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	if s.sessID != "" {
		t.Fatalf("registration unexpectedly succeeded (sessID %q); the test no longer exercises the fallback", s.sessID)
	}
	if s.sessionName() == "" {
		t.Error("display name is empty; the TUI, logs and daemon_info still need one")
	}
	if got := s.addressableName(); got != "" {
		t.Errorf("addressableName = %q, want empty for an unregistered session", got)
	}
	if got := s.inbox().Self; got != "" {
		t.Errorf("inbox Self = %q, want empty so Inbox.Claim treats the mailbox as off", got)
	}
}
