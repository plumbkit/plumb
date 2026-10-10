package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_for_task_roots_test.go — the root-keyed topology accessor, as the daemon
// wires it (PLAN-462 A4, Invariant 3): a pack and a hint are answered from the
// index of the calling agent's root, not the connection's.

const (
	rootMarkerA = "ALPHA-ONLY-MEMORY-MARKER-91c2"
	rootMarkerB = "BRAVO-ONLY-MEMORY-MARKER-4d7e"
)

// shopWorkspace copies the contextpack "shop" fixture into a fresh canonical
// directory that is a git project, so it can be pinned.
func shopWorkspace(t *testing.T) string {
	t.Helper()
	root := freshTempDir(t)
	if err := os.CopyFS(root, os.DirFS(filepath.Join("..", "tools", "testdata", "contextpack", "shop"))); err != nil {
		t.Fatal(err)
	}
	mustGitDir(t, root)
	return root
}

// shopFiles is how many files of the fixture the extractors index, as in
// internal/tools.
const shopFiles = 12

func waitIndexed(t *testing.T, store *topology.Store, files int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st := store.Status(); st.IndexedFiles >= files && st.IndexerState != "running" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d files were not indexed in time: %+v", files, store.Status())
}

// forRoot answers by the root it is given, finds a store attached under another
// spelling of the same place, and never opens one.
func TestTopologyPool_ForRootAnswersByCanonicalRootAndNeverOpensAStore(t *testing.T) {
	pool := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(pool.StopAll)
	target := freshTempDir(t)
	alias := filepath.Join(freshTempDir(t), "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store := pool.Acquire(alias, enabledTopologyConfig()) // attached by a non-canonical spelling
	if store == nil {
		t.Fatal("no store")
	}
	if got := pool.forRoot(target); got != store {
		t.Errorf("forRoot(canonical root) = %p, want the store attached through its alias %p", got, store)
	}
	if got := pool.forRoot(alias); got != store {
		t.Errorf("forRoot(the exact key) = %p, want %p", got, store)
	}
	if pool.forRoot("") != nil {
		t.Error("forRoot(\"\") answered")
	}
	if got := pool.forRoot(freshTempDir(t)); got != nil || pool.storeCount() != 1 {
		t.Errorf("forRoot(an unattached root) = %p with %d stores open; it must answer nil and open nothing", got, pool.storeCount())
	}
}

// The accessor the tool is registered with is keyed by the root asked for: the
// connection's own store answers only for the connection's root, and the pool's
// store answers for its own.
func TestConnSession_TopologyStoreForRootIsKeyedByRootNotByTheConnection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pool := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(pool.StopAll)
	s := newConnSession(context.Background(), detectTestPool(), pool, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	rootA, rootB := shopWorkspace(t), shopWorkspace(t)
	storeA, storeB := pool.Acquire(rootA, enabledTopologyConfig()), pool.Acquire(rootB, enabledTopologyConfig())
	s.mutate(func(v *sessionView) { v.acquiredRoot, v.topologyStore = rootA, storeA }) // the connection holds A

	if got := s.topologyStoreLive(); got != storeA {
		t.Fatalf("control: the connection's store is %p, want A's %p", got, storeA)
	}
	if got := s.topologyStoreForRoot(rootB); got != storeB {
		t.Errorf("topologyStoreForRoot(B) = %p, want B's store %p, not the connection's", got, storeB)
	}
	if got := s.topologyStoreForRoot(rootA); got != storeA {
		t.Errorf("topologyStoreForRoot(A) = %p, want A's store %p", got, storeA)
	}
	if s.topologyStoreForRoot("") != nil || s.topologyStoreForRoot(freshTempDir(t)) != nil || pool.storeCount() != 2 {
		t.Errorf("an unknown root must answer nil without opening a store (%d open)", pool.storeCount())
	}

	// With no pool the connection's own store still answers for its own root, and
	// for no other.
	bare := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(bare.close)
	bare.mutate(func(v *sessionView) { v.acquiredRoot, v.topologyStore = rootA, storeA })
	if bare.topologyStoreForRoot(rootA) != storeA || bare.topologyStoreForRoot(rootB) != nil {
		t.Error("without a pool the connection's store must answer for its own root only")
	}
}

// The real registration, two agents on one connection pinned to two roots. Each is
// answered from its own root's index and memories: A sees its private memory (the
// control), B never does, and B still resolves its own symbols, which the interim
// refusal of another root's index could not do.
func TestContextForTask_RealRegistration_TwoAgentsTwoRootsShareNoIndexAndNoMemory(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pool := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(pool.StopAll)
	s := newConnSession(context.Background(), detectTestPool(), pool, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	srv := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.registerAllTools(srv, time.Now())
	tool, ok := srv.Lookup("context_for_task")
	if !ok {
		t.Fatal("context_for_task is not registered")
	}
	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")

	rootA, rootB := shopWorkspace(t), shopWorkspace(t)
	memory := func(root, name, marker string) {
		if err := os.MkdirAll(filepath.Join(root, ".plumb", "memories"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: " + name + " rules\npaths: cart/*.go\n---\n\n" + marker + "\n"
		if err := os.WriteFile(filepath.Join(root, ".plumb", "memories", name+".md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	memory(rootA, "alpha-private", rootMarkerA)
	memory(rootB, "bravo-notes", rootMarkerB)

	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-b")
	for ctx, root := range map[context.Context]string{ctxA: rootA, ctxB: rootB} {
		if _, err := s.repinWorkspace(ctx, "file://"+root, "", false, false); err != nil {
			t.Fatal(err)
		}
	}
	// The daemon's pool holds an index per root; the connection holds A's.
	storeA, storeB := pool.Acquire(rootA, s.topologyConfigFor(rootA)), pool.Acquire(rootB, s.topologyConfigFor(rootB))
	waitIndexed(t, storeA, shopFiles)
	waitIndexed(t, storeB, shopFiles)
	s.mutate(func(v *sessionView) { v.topologyStore = storeA })

	raw, err := json.Marshal(map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}})
	if err != nil {
		t.Fatal(err)
	}
	outA, err := tool.Execute(ctxA, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outA, rootMarkerA) || !strings.Contains(outA, "body lines ") {
		t.Fatalf("control: agent A at its own root lacks its memory or body:\n%s", outA)
	}
	outB, err := tool.Execute(ctxB, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outB, rootMarkerB) || !strings.Contains(outB, "body lines ") || strings.Contains(outB, "no topology index") {
		t.Fatalf("agent B must resolve its own symbol from its own root's index and see its own memory:\n%s", outB)
	}
	for _, forbidden := range []string{rootMarkerA, "alpha-private", rootA} {
		if strings.Contains(outB, forbidden) {
			t.Errorf("agent B's pack contains %q, which belongs to root A:\n%s", forbidden, outB)
		}
	}
	if strings.Contains(outA, rootMarkerB) || strings.Contains(outA, rootB) {
		t.Errorf("agent A's pack contains root B's material:\n%s", outA)
	}
}

// The hint side as the daemon would build it from what it has: the pool's
// root-keyed accessor and a sensitive decision. A root with a store is answered from
// it; a root without one is answered with a label, and nothing is opened.
func TestContextHinter_BuiltFromThePoolAnswersByRoot(t *testing.T) {
	pool := newTopologyPool(enabledTopologyConfig())
	t.Cleanup(pool.StopAll)
	root := shopWorkspace(t)
	waitIndexed(t, pool.Acquire(root, enabledTopologyConfig()), shopFiles)
	h := tools.NewContextHinter(pool.forRoot, func(context.Context, string, string) bool { return false })

	res, err := h.Hint(t.Context(), tools.HintRequest{
		Workspace: root, Agent: "hooked", MaxBytes: 2048,
		Seeds: []tools.ContextSeed{{Path: "cart/cart.go", Symbol: "Cart.Total"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) < 2 || res.Lines[0].Selector != "(*Cart).Total" || res.Lines[0].Path != "cart/cart.go" || res.Freshness != "fresh" {
		t.Fatalf("a root with a store was not answered from it: %+v", res)
	}
	if !slices.ContainsFunc(res.Lines, func(l tools.HintLine) bool { return l.Selector == "Apply" && l.Path == "pricing/discount.go" }) {
		t.Errorf("the neighbourhood lacks Apply: %+v", res.Lines)
	}

	other := freshTempDir(t)
	none, err := h.Hint(t.Context(), tools.HintRequest{Workspace: other, Seeds: []tools.ContextSeed{{Symbol: "Total"}}, MaxBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Lines) != 0 || none.Freshness != "unavailable" || pool.storeCount() != 1 {
		t.Errorf("a root with no store: %+v with %d stores open; want a label and nothing opened", none, pool.storeCount())
	}
}
