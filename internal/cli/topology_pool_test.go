package cli

import (
	"testing"

	"github.com/golimpio/plumb/internal/config"
)

func enabledTopologyConfig() config.TopologyConfig {
	return config.TopologyConfig{
		Enabled:               true,
		MaxFileSizeBytes:      512 * 1024,
		ResyncBatch:           100,
		ResyncPauseMs:         25,
		ResyncIntervalMinutes: 60,
	}
}

func (p *topologyPool) storeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.stores)
}

func TestTopologyPool_ReconcileDisableClosesStores(t *testing.T) {
	dir := t.TempDir()
	p := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(p.StopAll)

	if s := p.Acquire(dir); s == nil {
		t.Fatal("expected a store from an enabled pool")
	}
	if got := p.storeCount(); got != 1 {
		t.Fatalf("store count = %d, want 1 after Acquire", got)
	}

	p.Reconcile(config.TopologyConfig{Enabled: false})

	if got := p.storeCount(); got != 0 {
		t.Errorf("store count = %d, want 0 after disable reconcile", got)
	}
}

func TestTopologyPool_ReconcileNoOpKeepsStore(t *testing.T) {
	dir := t.TempDir()
	cfg := enabledTopologyConfig()
	p := newTopologyPool(cfg)
	t.Cleanup(p.StopAll)

	s1 := p.Acquire(dir)
	if s1 == nil {
		t.Fatal("expected a store")
	}

	p.Reconcile(cfg) // identical config → no-op

	s2 := p.Acquire(dir)
	if s1 != s2 {
		t.Error("an unchanged reconcile must keep the same store instance")
	}
}

func TestTopologyPool_ReconcileReopensOnTuningChange(t *testing.T) {
	dir := t.TempDir()
	p := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(p.StopAll)

	s1 := p.Acquire(dir)
	if s1 == nil {
		t.Fatal("expected a store")
	}

	changed := enabledTopologyConfig()
	changed.MaxFileSizeBytes = 256 * 1024 // tuning change while still enabled
	p.Reconcile(changed)

	if got := p.storeCount(); got != 1 {
		t.Fatalf("store count = %d, want 1 after re-open", got)
	}
	s2 := p.Acquire(dir)
	if s1 == s2 {
		t.Error("a tuning change should re-open the store (new instance)")
	}
}
