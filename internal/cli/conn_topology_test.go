package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/topology"
)

// TestTopologyEnabledFor_ProjectOptOutWins is the F3 regression: with topology
// enabled globally (the default), an explicit per-project opt-out
// (<ws>/.plumb/config.toml [topology] enabled = false) must win. The old code
// gated startTopologyIndexer on the global value only, so a project could not
// opt out — contradicting the documented escape hatch.
func TestTopologyEnabledFor_ProjectOptOutWins(t *testing.T) {
	base := config.Defaults()
	base.Topology.Enabled = true
	s := &connSession{store: config.NewStore(base)}

	ws := t.TempDir()
	if !s.topologyEnabledFor(ws) {
		t.Fatal("no project config: the global default (enabled) should win")
	}

	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"),
		[]byte("[topology]\nenabled = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s.topologyEnabledFor(ws) {
		t.Error("project [topology] enabled = false must override the global default-on")
	}
}

// TestTopologyStoreLive_ReflectsLateAttach is the F4 regression: buildWriteDeps
// runs during tool registration, before the client handshake attaches the
// workspace. The topology write-notify must therefore resolve the store lazily
// — a store attached AFTER buildWriteDeps must still be visible, otherwise
// write-triggered re-indexing is silently dead (the index never reflects edits
// until the periodic resync).
func TestTopologyStoreLive_ReflectsLateAttach(t *testing.T) {
	s := &connSession{}
	if s.topologyStoreLive() != nil {
		t.Fatal("expected a nil store before attach")
	}

	wd := s.buildWriteDeps()
	if wd.TopologyNotify == nil {
		t.Fatal("TopologyNotify must be wired (non-nil) so writes can enqueue once attached")
	}
	wd.TopologyNotify("/before/attach.go") // nil store — must be a safe no-op

	st, err := topology.Open(t.TempDir(), config.Defaults().Topology, nil)
	if err != nil {
		t.Fatalf("opening topology store: %v", err)
	}
	defer func() { _ = st.Close() }()
	s.stateMu.Lock()
	s.topologyStore = st
	s.stateMu.Unlock()

	if s.topologyStoreLive() != st {
		t.Fatal("a store attached after buildWriteDeps must be visible via topologyStoreLive")
	}
	wd.TopologyNotify("/after/attach.go") // now resolves to the live store — must not panic
}

// TestReconcileTopologyStore_EnableDisable is the F5/F7 regression: a live config
// reload must refresh the session's topology store — acquiring one when enabled,
// and clearing it (not leaving a stale/closed handle) on a per-project opt-out —
// so topology enable/disable takes effect on the current session.
func TestReconcileTopologyStore_EnableDisable(t *testing.T) {
	base := config.Defaults()
	base.Topology.Enabled = true
	base.Topology.Watch = false // no OS watcher in a unit test
	ws := t.TempDir()
	s := &connSession{
		store:        config.NewStore(base),
		topologyPool: newTopologyPool(base.Topology),
	}
	defer s.topologyPool.StopAll()

	// Enabled globally, no project override -> the session acquires a store.
	s.reconcileTopologyStore(ws)
	if s.topologyStore == nil {
		t.Fatal("expected a topology store when enabled")
	}

	// Per-project opt-out -> the session clears its store, so its topology tools
	// report "disabled" rather than erroring on a stale handle.
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"),
		[]byte("[topology]\nenabled = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.reconcileTopologyStore(ws)
	if s.topologyStore != nil {
		t.Error("expected nil topology store after a per-project opt-out")
	}
}

// TestReconcileTopologyStore_ProjectTuningHonoured is the 0.8.34 regression:
// per-project [topology] *tuning* (not just enable/disable) must reach the
// store. The session passes the merged per-project config to the pool, so a
// project overriding e.g. max_file_size_bytes opens its store with that value
// rather than the global default — which the pool previously always used.
func TestReconcileTopologyStore_ProjectTuningHonoured(t *testing.T) {
	base := config.Defaults()
	base.Topology.Enabled = true
	base.Topology.Watch = false // no OS watcher in a unit test
	ws := t.TempDir()
	s := &connSession{
		store:        config.NewStore(base),
		topologyPool: newTopologyPool(base.Topology),
	}
	defer s.topologyPool.StopAll()

	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"),
		[]byte("[topology]\nmax_file_size_bytes = 131072\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.reconcileTopologyStore(ws)
	if s.topologyStore == nil {
		t.Fatal("expected a topology store when enabled")
	}
	if got := s.topologyPool.openedConfig(ws).MaxFileSizeBytes; got != 131072 {
		t.Errorf("store opened with max_file_size_bytes = %d, want the per-project 131072 (global default %d)", got, base.Topology.MaxFileSizeBytes)
	}
}
