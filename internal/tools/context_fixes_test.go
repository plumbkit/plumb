package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_fixes_test.go — the A1 review findings folded into A2: an exact path
// hint (B1), one classifier with its Shadowed disclosure (S1), the foreign-index
// guard (M1) and the smaller refusals.

// writeTree writes files (root-relative path to content) under root, creating
// directories.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// newGoWorkspace writes and indexes a small tree and wires the tool over it.
func newGoWorkspace(t *testing.T, files map[string]string) shopTool {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, files)
	return newShopTool(t, openContextStore(t, root, len(files)), root)
}

// B1: the path half of path#Selector is exact. Where one path is a suffix,
// a tail or a case variant of another, naming one must never resolve to the other.
func TestContextForTask_PathHintIsExactNotASubstring(t *testing.T) {
	const withF, withoutF = "package p\n\nfunc F() {}\n", "package p\n\nfunc G() {}\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		hint  string
		seed  string   // the one path that must resolve, or "" for none
		cands []string // the candidates a no-match offers
	}{
		{"root x.go lacks F, pkg/x.go has it", map[string]string{"x.go": withoutF, "pkg/x.go": withF}, "x.go#F", "", []string{"pkg/x.go"}},
		{"both have F: exactly the named one", map[string]string{"x.go": withF, "pkg/x.go": withF}, "x.go#F", "x.go", nil},
		{"a.go does not reach data.go", map[string]string{"a.go": withoutF, "data.go": withF}, "a.go#F", "", []string{"data.go"}},
		{"both have F: a.go is a.go", map[string]string{"a.go": withF, "data.go": withF}, "a.go#F", "a.go", nil},
		{"a directory selects its contents", map[string]string{"pkg/x.go": withF, "other/x.go": withF}, "pkg#F", "pkg/x.go", nil},
		{"a prefix of a directory name is not the directory", map[string]string{"pkg/x.go": withF}, "pk#F", "", []string{"pkg/x.go"}},
		{"a trailing slash still names the directory", map[string]string{"pkg/x.go": withF}, "pkg/#F", "pkg/x.go", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newGoWorkspace(t, tc.files)
			pack := s.collect(t, map[string]any{"symbols": []string{tc.hint}})
			if tc.seed != "" {
				if got := seedPaths(pack); !slices.Equal(got, []string{tc.seed}) || len(pack.Misses) != 0 {
					t.Fatalf("%s resolved to seeds %v, misses %+v; want exactly %s", tc.hint, got, pack.Misses, tc.seed)
				}
				return
			}
			if len(pack.Seeds) != 0 || len(pack.Misses) != 1 || pack.Misses[0].Ambiguous {
				t.Fatalf("%s: want one plain unresolved miss and no seed, got seeds=%v misses=%+v", tc.hint, seedPaths(pack), pack.Misses)
			}
			if got := candidatePaths(pack.Misses[0]); !slices.Equal(got, tc.cands) {
				t.Errorf("candidates = %v, want the labelled %v", got, tc.cands)
			}
			if !strings.Contains(pack.Misses[0].Reason, "candidates, not seeds") {
				t.Errorf("reason %q does not label the candidates as non-seeds", pack.Misses[0].Reason)
			}
		})
	}
}

// A path that differs from the indexed one only by case is not that path.
func TestContextForTask_PathHintIsCaseSensitive(t *testing.T) {
	s := newShop(t)
	// On a filesystem that folds case the path resolver may hand back the real
	// spelling, and then there is no case difference left to test.
	if rel := relWithinRoot(canonicalRoot(s.root), filepath.Join(canonicalRoot(s.root), "Cart", "cart.go")); rel != "Cart/cart.go" {
		t.Skipf("this filesystem canonicalises case (got %q), so the hint reaches the indexed spelling", rel)
	}
	pack := s.collect(t, map[string]any{"symbols": []string{"Cart/cart.go#Cart.Add"}})
	if len(pack.Seeds) != 0 || len(pack.Misses) != 1 {
		t.Fatalf("a case variant of the path resolved: seeds=%v misses=%+v", seedPaths(pack), pack.Misses)
	}
	if got := candidatePaths(pack.Misses[0]); !slices.Equal(got, []string{"cart/cart.go"}) {
		t.Errorf("candidates = %v, want the real cart/cart.go offered, labelled", got)
	}
}

// S1: references are set aside when a declaration matches, and the pack says so
// rather than letting the preference pass silently.
func TestContextForTask_ShadowedReferencesAreDisclosed(t *testing.T) {
	s := newGoWorkspace(t, map[string]string{
		"a/a.go": "package a\n\nimport \"stats\"\n\nvar _ = stats.X\n",
		"b/b.go": "package b\n\nfunc stats() int { return 1 }\n",
	})
	pack := s.collect(t, map[string]any{"symbols": []string{"stats"}})
	if len(pack.Seeds) != 1 || pack.Seeds[0].Path != "b/b.go" || pack.Seeds[0].Shadowed == 0 {
		t.Fatalf("want the one function with its shadowed references counted, got seeds=%+v misses=%+v", pack.Seeds, pack.Misses)
	}
	out, err := s.run(t, map[string]any{"symbols": []string{"stats"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "also matches " + referenceNodes(pack.Seeds[0].Shadowed)
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

// When only references match there is no declaration to seed: the answer is none,
// said as such, not an ambiguity among imports.
func TestContextForTask_OnlyReferencesIsNotASeed(t *testing.T) {
	s := newGoWorkspace(t, map[string]string{
		"a/a.go": "package a\n\nimport \"stats\"\n\nvar _ = stats.X\n",
		"b/b.go": "package b\n\nimport \"stats\"\n\nvar _ = stats.Y\n",
	})
	pack := s.collect(t, map[string]any{"symbols": []string{"stats"}})
	if len(pack.Seeds) != 0 || len(pack.Misses) != 1 || pack.Misses[0].Ambiguous || len(pack.Misses[0].Candidates) != 0 {
		t.Fatalf("want one plain miss with no candidates, got seeds=%v misses=%+v", seedPaths(pack), pack.Misses)
	}
	if !strings.Contains(pack.Misses[0].Reason, "a reference is not a declaration") {
		t.Errorf("reason %q does not say a reference is not a declaration", pack.Misses[0].Reason)
	}
}

// The collector and topology.ResolveSelector must not disagree about what a
// selector is, for every case that has a declaration to choose.
func TestContextForTask_ClassifiesAsTheTopologyResolverDoes(t *testing.T) {
	s := newShop(t)
	store := s.store
	for _, sel := range []string{"Total", "Cart.Add", "Apply", "Nonesuch"} {
		res, err := store.ResolveSelector(t.Context(), sel, topology.NodeHint{})
		if err != nil {
			t.Fatal(err)
		}
		pack := s.collect(t, map[string]any{"symbols": []string{sel}})
		switch res.Kind {
		case topology.ResolutionOne:
			if len(pack.Seeds) != 1 || pack.Seeds[0].Path != res.Node.Path {
				t.Errorf("%s: resolver says one (%s), collector gave seeds=%v misses=%+v", sel, res.Node.Path, seedPaths(pack), pack.Misses)
			}
		case topology.ResolutionAmbiguous:
			if len(pack.Seeds) != 0 || len(pack.Misses) != 1 || !pack.Misses[0].Ambiguous || len(pack.Misses[0].Candidates)+pack.Misses[0].More != len(res.Candidates) {
				t.Errorf("%s: resolver says ambiguous (%d), collector gave seeds=%v misses=%+v", sel, len(res.Candidates), seedPaths(pack), pack.Misses)
			}
		default:
			if len(pack.Seeds) != 0 || len(pack.Misses) != 1 || pack.Misses[0].Ambiguous {
				t.Errorf("%s: resolver says none, collector gave seeds=%v misses=%+v", sel, seedPaths(pack), pack.Misses)
			}
		}
	}
}

// M1, replaced by root-keyed access (Invariant 3): an index opened for one root says
// nothing true about another, so a root with no index of its own degrades instead
// of borrowing one. Symbol seeds are unresolved with a label, naming nothing of
// the other root; file seeds still work from the disk snapshot; the call is not
// refused.
func TestContextForTask_ARootWithNoIndexDegradesAndNeverBorrowsAnother(t *testing.T) {
	rootX, rootY := copyShopFixture(t), copyShopFixture(t)
	storeX := openShopStore(t, rootX)
	agentY := newShopTool(t, storeX, rootY) // the accessor answers for rootX only
	args := map[string]any{"files": []string{"cart/cart.go"}, "symbols": []string{"cart/cart.go#Cart.Add", "Total"}}

	pack := agentY.collect(t, args)
	if got := seedPaths(pack); !slices.Equal(got, []string{"cart/cart.go"}) {
		t.Fatalf("seeds = %v, want only the file seed", got)
	}
	if len(pack.Misses) != 2 {
		t.Fatalf("want both symbol seeds unresolved, got %+v", pack.Misses)
	}
	for _, m := range pack.Misses {
		if !strings.Contains(m.Reason, "no topology index is available for this root") || len(m.Candidates) != 0 {
			t.Errorf("miss %+v lacks the no-index label, or leaks candidates from another root's index", m)
		}
	}
	out, err := agentY.run(t, args)
	if err != nil {
		t.Fatalf("a root with no index must degrade, not be refused: %v", err)
	}
	if strings.Contains(out, canonicalRoot(rootX)) || strings.Contains(out, rootX) {
		t.Errorf("the other root's path is disclosed:\n%s", out)
	}
	if !strings.Contains(out, "topology index unavailable") || strings.Contains(out, "index health") {
		t.Errorf("the gap must say there is no index for this root, and report none of another's health:\n%s", out)
	}
	if !strings.Contains(out, "file cart/cart.go") {
		t.Errorf("the file seed must still be served from the disk:\n%s", out)
	}
	// Control: the same store served to the agent whose root it is resolves.
	agentX := newShopTool(t, storeX, rootX)
	if own := agentX.collect(t, args); len(own.Seeds) != 2 {
		t.Errorf("control: the index's own agent got seeds=%v misses=%+v", seedPaths(own), own.Misses)
	}
}

// Defence in depth: an accessor that hands back a store of some other root (the
// wiring mistake this invariant exists against) is not consulted, and the pack
// labels it without naming that root.
func TestContextForTask_AnAccessorThatReturnsAnotherRootsStoreIsNotConsulted(t *testing.T) {
	rootX, rootY := copyShopFixture(t), copyShopFixture(t)
	storeX := openShopStore(t, rootX)
	agentY := newShopTool(t, storeX, rootY)
	agentY.collector.storeFor = func(string) *topology.Store { return storeX } // ignores the root it was asked for
	args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Add"}}

	pack := agentY.collect(t, args)
	if len(pack.Seeds) != 0 || len(pack.Misses) != 1 || !strings.Contains(pack.Misses[0].Reason, "describes another root") {
		t.Fatalf("want one unresolved symbol labelled as another root's index, got seeds=%v misses=%+v", seedPaths(pack), pack.Misses)
	}
	out, err := agentY.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, canonicalRoot(rootX)) || strings.Contains(out, rootX) {
		t.Errorf("the other root's path is disclosed:\n%s", out)
	}
}

// The reason a symbol seed did not resolve carries the caller's path; like every
// other piece of caller-influenced text it is shown, not emitted.
func TestContextForTask_NoMatchReasonIsTerminalSafe(t *testing.T) {
	s := newShop(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"cart\x1b[2J.go#Cart.Add"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("a raw escape reached the output:\n%q", out)
	}
	if !strings.Contains(out, "no declaration matches within cart^[[2J.go") {
		t.Errorf("the reason should still name the path, escaped:\n%s", out)
	}
}

// within may name the root itself: it narrows nothing, and is not an error.
func TestContextForTask_WithinTheRootNarrowsNothing(t *testing.T) {
	s := newShop(t)
	for _, within := range []string{s.root, canonicalRoot(s.root), "."} {
		pack := s.collect(t, map[string]any{"files": []string{"cart/cart.go", "store/ledger.go"}, "within": []string{within}})
		if len(pack.Seeds) != 2 {
			t.Errorf("within %q narrowed the pack: seeds=%v misses=%+v", within, seedPaths(pack), pack.Misses)
		}
	}
	// Control: it still refuses a parent of the root, which would widen.
	_, err := s.run(t, map[string]any{"files": []string{"cart/cart.go"}, "within": []string{filepath.Dir(s.root)}})
	requireErr(t, err, "outside the workspace root")
}

// Plumb's own state is never a seed, by file or by symbol path; a memory reaches a
// pack through the memory corpus, not as a file.
func TestContextForTask_PlumbStateIsNeverASeed(t *testing.T) {
	s := newShop(t)
	writeFiles(t, s.root, map[string]string{
		".plumb/config.toml":           "[edits]\nstrict = true\n",
		".plumb/memories/decision.md":  "# Decision\n",
		".plumb/topology.db-wal":       "binary",
		".plumb/sessions/s1/state.txt": "x",
	})
	for _, file := range []string{".plumb/config.toml", ".plumb/memories/decision.md", ".plumb/topology.db-wal", ".plumb/sessions/s1/state.txt"} {
		for _, corpora := range [][]string{nil, {"memory"}} {
			args := map[string]any{"files": []string{file}}
			if corpora != nil {
				args["corpora"] = corpora
			}
			pack := s.collect(t, args)
			if len(pack.Seeds) != 0 || len(pack.Misses) != 1 || !strings.Contains(pack.Misses[0].Reason, "plumb's own state") {
				t.Errorf("%s (corpora %v): want a refusal naming plumb's own state, got seeds=%v misses=%+v", file, corpora, seedPaths(pack), pack.Misses)
			}
		}
	}
	pack := s.collect(t, map[string]any{"symbols": []string{".plumb/config.toml#Cart"}})
	if len(pack.Seeds) != 0 || len(pack.Misses) != 1 || !strings.Contains(pack.Misses[0].Reason, "plumb's own state") {
		t.Errorf("a symbol path under .plumb: want a refusal, got seeds=%v misses=%+v", seedPaths(pack), pack.Misses)
	}
	// Control: a neighbouring path that merely starts with the same text is a file.
	writeFiles(t, s.root, map[string]string{".plumbing/notes.txt": "x"})
	if ok := s.collect(t, map[string]any{"files": []string{".plumbing/notes.txt"}}); len(ok.Seeds) != 1 {
		t.Errorf("control: .plumbing/ was refused as plumb state: %+v", ok.Misses)
	}
}
