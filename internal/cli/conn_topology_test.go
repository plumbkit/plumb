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
