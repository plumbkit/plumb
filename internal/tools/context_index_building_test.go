package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// context_index_building_test.go — a pack read from an index on its first full
// pass says so (PLAN-547), and one read from a complete index the daemon has just
// reopened does not.

// pausedConfig paces the first pass to one file and then a pause far longer than
// any test, so a store opened with it stays on that pass, deterministically, until
// it is closed (the pause gives way to Stop).
var pausedConfig = config.TopologyConfig{MaxFileSizeBytes: 512 * 1024, ResyncBatch: 1, ResyncPauseMs: int(time.Hour / time.Millisecond)}

// openPausedStore opens root's index under pausedConfig and waits until the paused
// pass has indexed its first file and is running.
func openPausedStore(t *testing.T, root string) *topology.Store {
	t.Helper()
	s, err := topology.Open(root, pausedConfig,
		[]topology.Extractor{goext.New(), treesitter.NewPython(), treesitter.NewMarkdown()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := s.IndexedFileCount(context.Background())
		if h := s.Health(); n >= 1 && h.State == "running" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the paused pass never started: %+v", s.Health())
	return nil
}

// TestContextForTask_LabelsAPackFromAFirstBuild: an agent's first call in a fresh
// worktree reaches an index that holds a sliver of the tree. The pack leads with
// that, and the seed it could not resolve and the tests it could not find say they
// may simply not be indexed yet, rather than reading as a complete "none".
func TestContextForTask_LabelsAPackFromAFirstBuild(t *testing.T) {
	root := copyShopFixture(t)
	store := openPausedStore(t, root)
	if h := store.Health(); h.Built || h.Failing {
		t.Fatalf("control: a first pass must be neither built nor failing, got %+v", h)
	}
	s := newShopTool(t, store, root)
	pack := s.collect(t, map[string]any{"files": []string{"cart/cart.go"}, "symbols": []string{"pricing/discount.go#Apply"}, "intent": contextIntentChange})

	if len(pack.Gaps) == 0 || !strings.HasPrefix(pack.Gaps[0], "the topology index is still being built") {
		t.Fatalf("the pack does not lead with the first-build label: gaps = %q", pack.Gaps)
	}
	n, _ := store.IndexedFileCount(context.Background())
	if want := indexBuildingNote(n); pack.Gaps[0] != want {
		t.Errorf("lead gap = %q, want %q", pack.Gaps[0], want)
	}
	var miss *contextMiss
	for i := range pack.Misses {
		if pack.Misses[i].Input == "pricing/discount.go#Apply" {
			miss = &pack.Misses[i]
		}
	}
	if miss == nil || !miss.NoMatch || !strings.Contains(miss.Reason, "the index is still being built") {
		t.Errorf("the unindexed symbol seed does not say the index is being built: %+v", miss)
	}
	if !containsPrefix(pack.Gaps, "affected tests: the index lists none for these seeds so far, but it is still being built") {
		t.Errorf("the empty test estimate does not say the index is being built: gaps = %q", pack.Gaps)
	}
	if containsPrefix(pack.Gaps, "affected tests: the index lists none for these seeds; that is not proof") {
		t.Errorf("the empty test estimate also carries the complete-index wording: gaps = %q", pack.Gaps)
	}
	if out := renderContextPack(pack); !strings.Contains(out, "the topology index is still being built") {
		t.Error("the rendered pack drops the first-build label")
	}
}

// TestContextForTask_DoesNotLabelAReopenedCompleteIndex is the positive control:
// a complete index the daemon reopens runs its startup pass too, and while that
// pass runs nothing in memory has seen a pass finish (LastSync is zero). The pack
// must not call that index unbuilt: the completed pass is on disk.
func TestContextForTask_DoesNotLabelAReopenedCompleteIndex(t *testing.T) {
	root := copyShopFixture(t)
	first := openShopStore(t, root) // indexes the whole fixture and finishes its first pass
	if !first.Health().Built {
		t.Fatalf("control: a finished first pass must be recorded as built, got %+v", first.Health())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	store := openPausedStore(t, root) // the same index, reopened, its startup pass held
	h := store.Health()
	if !h.LastSync.IsZero() || h.State != "running" {
		t.Fatalf("control: the reopened index's startup pass must be in flight with no sync yet, got %+v", h)
	}
	if !h.Built {
		t.Fatalf("a reopened complete index reads as unbuilt: %+v", h)
	}
	s := newShopTool(t, store, root)
	pack := s.collect(t, map[string]any{"symbols": []string{"pricing/discount.go#Apply"}, "intent": contextIntentChange})
	for _, g := range pack.Gaps {
		if strings.Contains(g, "still being built") {
			t.Errorf("a reopened complete index is labelled as being built: %q", g)
		}
	}
	if len(pack.Seeds) == 0 {
		t.Error("control: the reopened index must resolve the seed its first pass indexed")
	}
}

func TestIndexBuilding(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    topology.Health
		want bool
	}{
		{"first pass running", topology.Health{State: "running"}, true},
		{"before the first pass is picked up", topology.Health{State: "idle"}, true},
		{"built, startup pass after a restart", topology.Health{State: "running", Built: true}, false},
		{"built and idle", topology.Health{State: "idle", Built: true, LastSync: time.Now()}, false},
		{"failing (worded by the health note instead)", topology.Health{State: "error", Failing: true}, false},
		{"no indexer attached", topology.Health{State: "stopped"}, false},
	} {
		if got := indexBuilding(tc.h); got != tc.want {
			t.Errorf("%s: indexBuilding(%+v) = %v, want %v", tc.name, tc.h, got, tc.want)
		}
	}
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
