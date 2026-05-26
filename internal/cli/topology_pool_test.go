package cli

import (
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
// the intentional tsx alias: the regex TSX/JSX extractor labels its nodes
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
