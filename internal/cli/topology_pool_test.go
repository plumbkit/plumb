package cli

import (
	"reflect"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/langsupport"
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

// openedConfig returns the effective config the store for root was opened with.
func (p *topologyPool) openedConfig(root string) config.TopologyConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfgs[root]
}

func TestTopologyPool_ReconcileDisableClosesStores(t *testing.T) {
	dir := t.TempDir()
	p := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(p.StopAll)

	if s := p.Acquire(dir, enabledTopologyConfig()); s == nil {
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

	s1 := p.Acquire(dir, cfg)
	if s1 == nil {
		t.Fatal("expected a store")
	}

	p.Reconcile(cfg) // identical config → no-op

	s2 := p.Acquire(dir, cfg)
	if s1 != s2 {
		t.Error("an unchanged reconcile must keep the same store instance")
	}
}

func TestTopologyPool_ReconcileReopensOnTuningChange(t *testing.T) {
	dir := t.TempDir()
	p := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(p.StopAll)

	s1 := p.Acquire(dir, enabledTopologyConfig())
	if s1 == nil {
		t.Fatal("expected a store")
	}

	changed := enabledTopologyConfig()
	changed.MaxFileSizeBytes = 256 * 1024 // tuning change while still enabled
	p.Reconcile(changed)

	if got := p.storeCount(); got != 1 {
		t.Fatalf("store count = %d, want 1 after re-open", got)
	}
	s2 := p.Acquire(dir, changed)
	if s1 == s2 {
		t.Error("a tuning change should re-open the store (new instance)")
	}
}

// TestTopologyPool_AcquireReopensOnConfigChange is the per-project-config
// regression: Acquire opens the store with the caller's merged config, and a
// later Acquire carrying a different config (the per-project tuning case) closes
// and re-opens the store so the new settings take effect — rather than silently
// returning the store opened with the global config.
func TestTopologyPool_AcquireReopensOnConfigChange(t *testing.T) {
	dir := t.TempDir()
	p := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(p.StopAll)

	s1 := p.Acquire(dir, enabledTopologyConfig())
	if s1 == nil {
		t.Fatal("expected a store")
	}

	projectTuned := enabledTopologyConfig()
	projectTuned.MaxFileSizeBytes = 256 * 1024 // per-project override
	s2 := p.Acquire(dir, projectTuned)
	if s2 == nil {
		t.Fatal("expected a store after re-acquire")
	}
	if s1 == s2 {
		t.Error("Acquire with a different per-project config must re-open the store")
	}
	if got := p.openedConfig(dir); !reflect.DeepEqual(got, projectTuned) {
		t.Errorf("pool tracked config = %+v, want the per-project config %+v", got, projectTuned)
	}
	if got := p.storeCount(); got != 1 {
		t.Errorf("store count = %d, want 1", got)
	}
}

// TestTopologyPool_AcquireSameConfigReturnsSameStore pins the idempotent fast
// path: re-acquiring a root with an unchanged config must reuse the instance,
// not churn the store.
func TestTopologyPool_AcquireSameConfigReturnsSameStore(t *testing.T) {
	dir := t.TempDir()
	cfg := enabledTopologyConfig()
	p := newTopologyPool(cfg)
	t.Cleanup(p.StopAll)

	s1 := p.Acquire(dir, cfg)
	s2 := p.Acquire(dir, cfg)
	if s1 == nil || s1 != s2 {
		t.Error("Acquire with an unchanged config must return the same store instance")
	}
}

// TestBuildExtractorsCoversRegistry guards the silent-failure seam: buildExtractors
// only indexes a language when its langsupport row has a matching extractorCtors
// entry. A row added without a constructor would stop being indexed with no error,
// so this fails loudly if either side drifts.
func TestBuildExtractorsCoversRegistry(t *testing.T) {
	for _, l := range langsupport.All() {
		if l.Structural == langsupport.EngineNone {
			continue
		}
		if _, ok := extractorCtors[l.Name]; !ok {
			t.Errorf("langsupport row %q (engine %v) has no extractorCtors entry — its files would never be indexed", l.Name, l.Structural)
		}
	}
	for name := range extractorCtors {
		l, ok := langsupport.ByName(name)
		if !ok {
			t.Errorf("extractorCtors[%q] has no langsupport row", name)
			continue
		}
		if l.Structural == langsupport.EngineNone {
			t.Errorf("extractorCtors[%q] maps to a langsupport row with EngineNone, so buildExtractors never builds it", name)
		}
	}
}

// TestExtractorRegistryAlignment pins each extractor's Extensions() to its
// langsupport row and its Language() label. The labels match the row Name except
// the intentional tsx alias: the WASM TSX/JSX extractor labels its nodes
// "typescript" so .ts and .tsx symbols search together under one language.
func TestExtractorRegistryAlignment(t *testing.T) {
	langOverride := map[string]string{"tsx": "typescript"}
	for name, ctor := range extractorCtors {
		ex := ctor()
		row, ok := langsupport.ByName(name)
		if !ok {
			t.Errorf("extractorCtors[%q]: no langsupport row", name)
			continue
		}
		want := name
		if o, has := langOverride[name]; has {
			want = o
		}
		if ex.Language() != want {
			t.Errorf("extractor %q Language() = %q, want %q", name, ex.Language(), want)
		}
		if !slices.Equal(ex.Extensions(), row.Extensions) {
			t.Errorf("extractor %q Extensions() = %v, want %v (langsupport row)", name, ex.Extensions(), row.Extensions)
		}
	}
}
