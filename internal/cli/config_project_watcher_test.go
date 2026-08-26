package cli

// config_project_watcher_test.go — PLAN-414 coverage for the daemon-owned
// per-workspace project-config watcher and its session integration.
//
// The manager tests use a short debounce and a signalling dispatch closure so
// every wait is on the watcher's own signal, never a sleep. The two-session
// test is the yayl-shaped regression from the card: global cross_project=false,
// trusted project true, no daemon restart and no new connection.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

// testWatchManager builds a manager with a fast debounce and a dispatch that
// forwards to registry.reloadProject AND signals the returned channel once per
// dispatch — the deterministic "watcher fired" signal the tests wait on.
func testWatchManager(t *testing.T, registry *connRegistry) (*projectConfigWatchManager, chan string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sig := make(chan string, 64)
	m := newProjectConfigWatchManager(ctx, func(ws string) {
		if registry != nil {
			registry.reloadProject(ws)
		}
		sig <- ws
	})
	m.debounce = 30 * time.Millisecond
	t.Cleanup(m.close)
	return m, sig
}

// awaitDispatch fails the test unless a dispatch for want arrives within the
// timeout. Dispatches for OTHER workspaces are consumed and reported.
func awaitDispatch(t *testing.T, sig <-chan string, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-sig:
			if got == want {
				return
			}
			t.Logf("dispatch for %q while awaiting %q", got, want)
		case <-deadline:
			t.Fatalf("no watcher dispatch for %s within 10s", want)
		}
	}
}

// noDispatchWithin asserts the watcher stays silent for a window well past the
// debounce — the negative half of the isolation and debounce contracts.
func noDispatchWithin(t *testing.T, sig <-chan string, d time.Duration) {
	t.Helper()
	select {
	case ws := <-sig:
		t.Fatalf("unexpected watcher dispatch for %s", ws)
	case <-time.After(d):
	}
}

func writeProjectCfg(t *testing.T, ws, body string) {
	t.Helper()
	dir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectWatchManager_OneWatcherPerWorkspace(t *testing.T) {
	m, _ := testWatchManager(t, nil)
	ws := t.TempDir()

	m.acquire(ws)
	m.acquire(ws)
	if got := m.refs(ws); got != 2 {
		t.Fatalf("refs = %d, want 2", got)
	}
	// A trailing-slash alias is the SAME workspace, not a second watcher.
	m.acquire(ws + string(filepath.Separator))
	if got := m.refs(ws); got != 3 {
		t.Fatalf("refs after aliased acquire = %d, want 3", got)
	}
	if !m.healthy(ws) {
		t.Error("watcher should be healthy after acquire")
	}
	m.release(ws)
	m.release(ws)
	if got := m.refs(ws); got != 1 {
		t.Fatalf("refs after two releases = %d, want 1", got)
	}
	m.release(ws)
	if got := m.refs(ws); got != 0 {
		t.Fatalf("refs after last release = %d, want 0 — watcher must be torn down", got)
	}
	if m.healthy(ws) {
		t.Error("healthy = true after the last release; the watcher is gone")
	}
}

func TestProjectWatchManager_SymlinkAliasSharesWatcher(t *testing.T) {
	m, _ := testWatchManager(t, nil)
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m.acquire(realDir)
	m.acquire(link)
	if got := m.refs(realDir); got != 2 {
		t.Fatalf("refs = %d, want 2 — symlink and real path must share one watcher", got)
	}
	m.release(realDir)
	m.release(link)
	if got := m.refs(realDir); got != 0 {
		t.Fatalf("refs = %d, want 0 after both spellings released", got)
	}
}

func TestProjectWatchManager_DispatchesOnWrite(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	ws := t.TempDir()
	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	m.acquire(ws)

	writeProjectCfg(t, ws, "[edits]\nstrict = false\n")
	awaitDispatch(t, sig, paths.Canonical(ws))
}

func TestProjectWatchManager_DispatchesOnDelete(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	ws := t.TempDir()
	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	m.acquire(ws)

	if err := os.Remove(filepath.Join(ws, ".plumb", "config.toml")); err != nil {
		t.Fatal(err)
	}
	awaitDispatch(t, sig, paths.Canonical(ws))
}

func TestProjectWatchManager_ConfigCreatedAfterAttach(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	ws := t.TempDir() // no .plumb yet — the watcher must see it appear
	m.acquire(ws)

	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	awaitDispatch(t, sig, paths.Canonical(ws))
}

func TestProjectWatchManager_DebounceBurstCollapsesToOneDispatch(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	ws := t.TempDir()
	writeProjectCfg(t, ws, "")
	m.acquire(ws)

	for range 5 {
		writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	}
	awaitDispatch(t, sig, paths.Canonical(ws))
	// The burst must collapse; a second dispatch within several debounce
	// windows means events are not being coalesced.
	noDispatchWithin(t, sig, 5*m.debounce+100*time.Millisecond)
}

func TestProjectWatchManager_NeverReloadsAnotherWorkspace(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	wsA, wsB := t.TempDir(), t.TempDir()
	writeProjectCfg(t, wsA, "")
	m.acquire(wsA)
	m.acquire(wsB)

	writeProjectCfg(t, wsA, "[edits]\nstrict = true\n")
	awaitDispatch(t, sig, paths.Canonical(wsA))
	// B has no config and was never touched: any further dispatch is a leak.
	noDispatchWithin(t, sig, 5*m.debounce+100*time.Millisecond)
}

func TestProjectWatchManager_NoDispatchAfterLastRelease(t *testing.T) {
	m, sig := testWatchManager(t, nil)
	ws := t.TempDir()
	writeProjectCfg(t, ws, "")
	m.acquire(ws)
	m.release(ws)

	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	noDispatchWithin(t, sig, 5*m.debounce+100*time.Millisecond)
}

func TestCollabChangeNotice(t *testing.T) {
	tests := []struct {
		name       string
		prev, next config.CollabConfig
		want       string // substring; "" means no notice
	}{
		{"no change", config.CollabConfig{Mailbox: true}, config.CollabConfig{Mailbox: true}, ""},
		{
			"tuning-only change is silent",
			config.CollabConfig{MaxExchanges: 10},
			config.CollabConfig{MaxExchanges: 3},
			"",
		},
		{
			"cross_project enabled",
			config.CollabConfig{},
			config.CollabConfig{CrossProject: true},
			"now enabled: cross_project",
		},
		{
			"mailbox revoked",
			config.CollabConfig{Mailbox: true},
			config.CollabConfig{},
			"now revoked: mailbox",
		},
		{
			"mixed",
			config.CollabConfig{Mailbox: true},
			config.CollabConfig{Intents: true},
			"enabled: intents; now revoked: mailbox",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collabChangeNotice(tt.prev, tt.next)
			if tt.want == "" {
				if got != "" {
					t.Errorf("notice = %q, want none", got)
				}
				return
			}
			if got == "" || !strings.Contains(got, tt.want) {
				t.Errorf("notice = %q, want substring %q", got, tt.want)
			}
		})
	}
}

// watchTestSession builds a session wired like handleConn wires it: store,
// watch manager, and a registry entry whose reload hook re-applies the pinned
// workspace's project config.
func watchTestSession(t *testing.T, ctx context.Context, store *config.Store, registry *connRegistry, m *projectConfigWatchManager, id, ws string) *connSession {
	t.Helper()
	s := &connSession{ctx: ctx, store: store, projectWatches: m}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	registry.add(id, connHandle{
		workspace:     s.workspace,
		reloadProject: func() { s.applyProjectConfig(s.workspace()) },
	})
	t.Cleanup(func() { registry.remove(id) })
	s.applyProjectConfig(ws)
	return s
}

// TestProjectConfigWatcher_TwoSessionsReloadLive is the PLAN-414 regression:
// two sessions already attached to one workspace, global cross_project=false.
// Writing (and trusting) a project override must flip BOTH sessions' live
// collab policy through the watcher — no reconnect, no daemon restart, no
// global edit. Revocation (deletion) must reach both, fail-closed. And an
// UNtrusted project asking for cross_project must change nothing.
func TestProjectConfigWatcher_TwoSessionsReloadLive(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // trust store is a temp file
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	root := paths.Canonical(ws)
	writeProjectCfg(t, ws, "[collab]\ncross_project = false\n")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := config.NewStore(config.Defaults()) // global cross_project = false
	registry := newConnRegistry()
	m, sig := testWatchManager(t, registry)

	s1 := watchTestSession(t, ctx, store, registry, m, "s1", ws)
	s2 := watchTestSession(t, ctx, store, registry, m, "s2", ws)
	for i, s := range []*connSession{s1, s2} {
		if s.collabConfig().CrossProject {
			t.Fatalf("session %d: cross_project on before the override exists", i)
		}
	}
	if got := m.refs(ws); got != 2 {
		t.Fatalf("watcher refs = %d, want 2 (one per attached session)", got)
	}

	// An UNTRUSTED project asking for cross_project must change nothing, even
	// though the watcher fires and both sessions re-apply.
	writeProjectCfg(t, ws, "[collab]\ncross_project = true\n")
	awaitDispatch(t, sig, root)
	for i, s := range []*connSession{s1, s2} {
		if s.collabConfig().CrossProject {
			t.Fatalf("session %d: untrusted project config granted cross_project", i)
		}
	}

	// Trust the current content (what `plumb trust` records) — now the same
	// file takes effect on the next watcher tick.
	grantExecTrust(t, ws)
	writeProjectCfg(t, ws, "[collab]\ncross_project = true\n")
	awaitDispatch(t, sig, root)
	for i, s := range []*connSession{s1, s2} {
		if !s.collabConfig().CrossProject {
			t.Fatalf("session %d: trusted cross_project=true did not reach the live session", i)
		}
	}
	// The agent on each session is told, once.
	if n := s1.collabPolicyNotice(); n == "" || !strings.Contains(n, "cross_project") {
		t.Errorf("s1 notice = %q, want a cross_project policy-change notice", n)
	}
	if n := s1.collabPolicyNotice(); n != "" {
		t.Errorf("s1 notice surfaced twice: %q", n)
	}

	// Revocation by deletion is fail-closed and reaches both sessions.
	if err := os.Remove(filepath.Join(ws, ".plumb", "config.toml")); err != nil {
		t.Fatal(err)
	}
	awaitDispatch(t, sig, root)
	for i, s := range []*connSession{s1, s2} {
		if s.collabConfig().CrossProject {
			t.Fatalf("session %d: cross_project survived deletion of the file that granted it", i)
		}
	}
}

// TestReconcileProjectConfig_FallbackOnlyWhenWatcherUnhealthy pins the poll's
// new role: with a healthy watcher it is a no-op; with a failed/absent one it
// reconciles exactly as the old 30s poll did.
func TestReconcileProjectConfig_FallbackOnlyWhenWatcherUnhealthy(t *testing.T) {
	ws := t.TempDir()
	writeProjectCfg(t, ws, "[edits]\nstrict = false\n")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := config.NewStore(config.Defaults())
	m, _ := testWatchManager(t, nil)
	s := &connSession{ctx: ctx, store: store, projectWatches: m}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.applyProjectConfig(ws)

	// Watcher healthy: a file change the watcher has not dispatched yet is NOT
	// the poll's business.
	writeProjectCfg(t, ws, "[edits]\nstrict = true\n")
	s.reconcileProjectConfig()
	if s.isStrict() {
		t.Error("poll applied the config while the watcher was healthy — it must defer to the watcher")
	}

	// Watcher failed: the poll owns the workspace again.
	m.mu.Lock()
	m.watches[paths.Canonical(ws)].failed.Store(true)
	m.mu.Unlock()
	s.reconcileProjectConfig()
	if !s.isStrict() {
		t.Error("poll did not reconcile while the watcher was failed")
	}
	if !s.view().fallbackWarned {
		t.Error("fallbackWarned not latched; the once-per-engagement warning could flap every 30s")
	}

	// No manager wired at all (unit-test construction): the poll still works,
	// exactly as before PLAN-414.
	s2 := &connSession{ctx: ctx, store: store}
	s2.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	writeProjectCfg(t, ws, "[edits]\nstrict = false\n")
	s2.reconcileProjectConfig()
	if s2.view().lastCfgMtime.IsZero() {
		t.Error("poll with no manager did not run checkAndReloadConfig")
	}
}
