package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// seedSelectors lists a pack's seeds as path#selector (symbols) or path (files),
// with their origin, in seed order.
func seedSelectors(p contextPack) []string {
	out := make([]string, 0, len(p.Seeds))
	for _, s := range p.Seeds {
		id := s.Path
		if s.Kind == seedSymbol {
			id += "#" + s.Selector
		}
		out = append(out, id+" <"+s.Origin+">")
	}
	return out
}

// A request with only task prose is answered: the declarations whose names carry
// the task's words become seeds, each labelled as found by search, with the words
// it matched.
func TestContextForTask_Discover_ProseAloneFindsDeclarationsByName(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"task": "the checkout charges the ledger twice"})
	if pack.Discovery == nil {
		t.Fatal("a prose-only request must record its discovery")
	}
	got := seedSelectors(pack)
	if !slices.Contains(got, "api/checkout.go#Checkout <"+originFound+">") {
		t.Fatalf("seeds %v do not include the Checkout declaration found by search", got)
	}
	for _, sd := range pack.Seeds {
		if sd.Origin != originFound {
			t.Errorf("seed %s has origin %q, want %q", sd.Selector, sd.Origin, originFound)
		}
		if sd.Matched == "" {
			t.Errorf("seed %s does not say which task words it matched", sd.Selector)
		}
	}
	out, err := s.run(t, map[string]any{"task": "the checkout charges the ledger twice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"proposed from the task prose", originFound + " on: checkout"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered pack does not say %q:\n%s", want, out)
		}
	}
}

// Code the task names outright comes first and is labelled as named: a
// backticked selector and a path to an existing file.
func TestContextForTask_Discover_NamedCodeIsSeededAndLabelled(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"task": "Why does `Cart.Total` ignore the code? See pricing/discount.go."})
	got := seedSelectors(pack)
	want := []string{"cart/cart.go#(*Cart).Total <" + originNamed + ">", "pricing/discount.go <" + originNamed + ">"}
	if len(got) < 2 || !slices.Equal(got[:2], want) {
		t.Fatalf("seeds = %v, want the named ones first: %v", got, want)
	}
	// Search never adds a declaration from a file the task already named.
	for _, sd := range pack.Seeds[2:] {
		if sd.Path == "pricing/discount.go" {
			t.Errorf("search added %s#%s from a file the task named", sd.Path, sd.Selector)
		}
	}
}

// Explicit seeds win: prose then only re-ranks, and nothing is proposed.
func TestContextForTask_Discover_ExplicitSeedsAreNeverSupplemented(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"files": []string{"store/ledger.go"}, "task": "the checkout charges twice"})
	if pack.Discovery != nil {
		t.Error("a request with explicit seeds must not run discovery")
	}
	if got := seedSelectors(pack); !slices.Equal(got, []string{"store/ledger.go <>"}) {
		t.Errorf("seeds = %v, want only the explicit file", got)
	}
}

// Discovery honours within: nothing outside the narrowed scope is proposed, as a
// seed or as a candidate. The control without within proves the out-of-scope
// declaration would otherwise be found.
func TestContextForTask_Discover_RespectsWithin(t *testing.T) {
	s := newShop(t)
	free := s.collect(t, map[string]any{"task": "cart total"})
	if !slices.ContainsFunc(free.Seeds, func(sd contextSeed) bool { return sd.Path == "cart/cart.go" }) {
		t.Fatalf("control: without within, cart/cart.go should be found; seeds %v", seedSelectors(free))
	}
	pack := s.collect(t, map[string]any{"task": "cart total", "within": []string{"store/**"}})
	for _, sd := range pack.Seeds {
		if !strings.HasPrefix(sd.Path, "store/") {
			t.Errorf("seed %s is outside within", sd.Path)
		}
	}
	for _, c := range pack.Discovery.Others {
		if !strings.HasPrefix(c.Path, "store/") {
			t.Errorf("candidate %s is outside within", c.Path)
		}
	}
	if len(pack.Seeds) == 0 {
		t.Errorf("Ledger.Total is inside within and carries \"total\"; seeds %v", seedSelectors(pack))
	}
}

// At most contextMaxDiscovered seeds are proposed; the next matches are listed as
// candidates and are never also seeds. Two calls propose the same seeds.
func TestContextForTask_Discover_CapsSeedsAndListsTheRestAsCandidates(t *testing.T) {
	s := newShop(t)
	args := map[string]any{"task": "total checkout ledger discount apply lookup record summarise"}
	pack := s.collect(t, args)
	if len(pack.Seeds) != contextMaxDiscovered {
		t.Fatalf("got %d seeds %v, want the cap %d", len(pack.Seeds), seedSelectors(pack), contextMaxDiscovered)
	}
	if len(pack.Discovery.Others) == 0 {
		t.Fatal("matches beyond the cap must be listed as candidates")
	}
	for _, c := range pack.Discovery.Others {
		for _, sd := range pack.Seeds {
			if c.Path == sd.Path && c.Selector == sd.Selector {
				t.Errorf("%s#%s is both a seed and a candidate", c.Path, c.Selector)
			}
		}
	}
	again := s.collect(t, args)
	if !slices.Equal(seedSelectors(pack), seedSelectors(again)) {
		t.Errorf("discovery is not deterministic: %v then %v", seedSelectors(pack), seedSelectors(again))
	}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "other matches (candidates, not seeds):") {
		t.Errorf("rendered pack does not list the other matches:\n%s", out)
	}
}

// Prose that matches nothing, or has no searchable words, is answered with a
// pack that says so and points at the alternatives, not with an error.
func TestContextForTask_Discover_NothingFoundIsSaid(t *testing.T) {
	s := newShop(t)
	cases := []struct{ task, want string }{
		{"zebra quantum flux", "no declaration name carries the task's words (zebra quantum flux)"},
		{"fix the and of it", "the task prose has no searchable words"},
	}
	for _, c := range cases {
		out, err := s.run(t, map[string]any{"task": c.task})
		if err != nil {
			t.Fatalf("%q: %v", c.task, err)
		}
		if !strings.Contains(out, c.want) || !strings.Contains(out, "seeds (0 resolved") {
			t.Errorf("%q: pack does not say %q:\n%s", c.task, c.want, out)
		}
	}
}

// A path the task names that the boundary guard refuses drops only that seed,
// with the reason, instead of failing the call: the tool proposed it, so it is
// not the caller's argument to fix. An explicit seed with the same path is still
// refused (the control).
func TestContextForTask_Discover_ARefusedProposalIsAMissNotAnError(t *testing.T) {
	s := newShop(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := s.run(t, map[string]any{"files": []string{"escape/x.go"}}); err == nil {
		t.Fatal("control: an explicit seed through an escaping symlink must be refused")
	}
	pack := s.collect(t, map[string]any{"task": "look at escape/x.go and the cart total"})
	if !slices.ContainsFunc(pack.Misses, func(m contextMiss) bool {
		return m.Input == "escape/x.go" && strings.Contains(m.Reason, "proposed from the task prose")
	}) {
		t.Errorf("the refused proposal is not reported as a miss: %+v", pack.Misses)
	}
}

func TestPromptSelectors(t *testing.T) {
	got := PromptSelectors("fix `Cart.Total`, `go test` and `cart/cart.go`; also api/checkout.go:12 and `Cart.Total`.", 8)
	want := []string{"Cart.Total", "cart/cart.go", "api/checkout.go"}
	if !slices.Equal(got, want) {
		t.Errorf("PromptSelectors = %v, want %v", got, want)
	}
	if got := PromptSelectors("`a.b` `c.d` `e.f`", 2); len(got) != 2 {
		t.Errorf("max is not honoured: %v", got)
	}
}

func TestDiscoverTerms(t *testing.T) {
	got := discoverTerms("Fix the parseConfig error when the TOML_file is empty")
	want := []string{"parseconfig", "parse", "config", "error", "toml_file", "toml", "file", "empty"}
	if !slices.Equal(got, want) {
		t.Errorf("discoverTerms = %v, want %v", got, want)
	}
	got = discoverTerms("daemon restarts lose the class")
	want = []string{"daemon", "restarts", "restart", "lose", "class"}
	if !slices.Equal(got, want) {
		t.Errorf("discoverTerms = %v, want %v", got, want)
	}
}

// Words match by stem, each stem counts once, and a declaration in a test file
// ranks below the code it tests unless the task speaks of tests.
func TestRankDiscovered_StemsAndTestDemotion(t *testing.T) {
	scope := contextScope{root: "/r"}
	hit := func(name, p string) topology.SearchResult {
		return topology.SearchResult{Node: topology.Node{Name: name, Qualified: name, Path: p, Kind: topology.KindFunction}}
	}
	hits := []topology.SearchResult{hit("restartDaemon", "d/d_test.go"), hit("restartDaemon", "d/d.go")}
	got, _ := rankDiscovered(hits, discoverTerms("daemon restarts"), scope)
	if len(got) != 2 || got[0].node.Path != "d/d.go" {
		t.Fatalf("production code must outrank its test: %+v", got)
	}
	if got[0].score != 4 {
		t.Errorf("restartDaemon scores %d on \"daemon restarts\", want 4 (two name stems, \"restart\" counted once)", got[0].score)
	}
	if got[1].score != 3 {
		t.Errorf("the test-file twin scores %d, want 3 (one point off)", got[1].score)
	}
	withTests, _ := rankDiscovered(hits, discoverTerms("daemon restarts in tests"), scope)
	if withTests[0].node.Path != "d/d_test.go" {
		t.Errorf("a task about tests must not demote test files (its path even carries \"test\"): %+v", withTests)
	}
}

// A word matches a name through its stem: "updating" carries "update".
func TestRankDiscovered_StemMatchesInflections(t *testing.T) {
	hits := []topology.SearchResult{{Node: topology.Node{Name: "updateLedger", Qualified: "updateLedger", Path: "s/s.go", Kind: topology.KindFunction}}}
	got, _ := rankDiscovered(hits, discoverTerms("updating ledgers"), contextScope{root: "/r"})
	if len(got) != 1 || got[0].score != 4 {
		t.Fatalf("updateLedger on \"updating ledgers\" = %+v, want score 4 (both words in the name, by stem)", got)
	}
}

// A file the task names is its own seed: search does not add declarations from
// it as further seeds. The control shows search would otherwise pick them.
func TestContextForTask_Discover_NamedFileIsNotMinedForMoreSeeds(t *testing.T) {
	s := newShop(t)
	control := s.collect(t, map[string]any{"task": "apply the discount lookup"})
	if !slices.ContainsFunc(control.Seeds, func(sd contextSeed) bool { return sd.Path == "pricing/discount.go" }) {
		t.Fatalf("control: search should seed from pricing/discount.go; seeds %v", seedSelectors(control))
	}
	pack := s.collect(t, map[string]any{"task": "apply the discount lookup in pricing/discount.go"})
	n := 0
	for _, sd := range pack.Seeds {
		if sd.Path == "pricing/discount.go" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("pricing/discount.go seeded %d times, want once (the named file): %v", n, seedSelectors(pack))
	}
}

// A word that only a path carries is too weak to seed from: the declarations are
// listed as candidates, never seeds.
func TestContextForTask_Discover_PathOnlyMatchesNeverSeed(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"task": "pricing"})
	if len(pack.Seeds) != 0 {
		t.Errorf("a path-only match seeded %v", seedSelectors(pack))
	}
	if pack.Discovery == nil || len(pack.Discovery.Others) == 0 {
		t.Error("the path-only matches must still be listed as candidates")
	}
}

// Discovery is the tool's choice, so it is held to what expansion is held to: it
// never proposes a seed in a sensitive file, by search or by a path the task
// names, and the marker never reaches the response. Control: with no decision
// wired, the same task does seed the vault (so the guard is what stops it).
func TestContextForTask_Discover_NeverProposesASensitiveFile(t *testing.T) {
	s := newSensitiveFixture(t)
	task := map[string]any{"task": "rotate the vault token credential"}
	open := s.collect(t, task)
	if !slices.ContainsFunc(open.Seeds, func(sd contextSeed) bool { return sd.Path == "vault/secrets.go" }) {
		t.Fatalf("control: without the decision the vault should be found; seeds %v", seedSelectors(open))
	}
	rule := &defaultSensitive{root: s.root}
	s.collector.WithSensitive(rule.fn)
	for _, args := range []map[string]any{task, {"task": "look at ./vault/secrets.go and vault/secrets.go"}} {
		pack := s.collect(t, args)
		for _, sd := range pack.Seeds {
			if sd.Path == "vault/secrets.go" {
				t.Errorf("%v: proposed a seed in a sensitive file: %v", args, seedSelectors(pack))
			}
		}
		out, err := s.run(t, args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, sensitiveMarker) {
			t.Errorf("%v: sensitive content reached the response:\n%s", args, out)
		}
	}
	pack := s.collect(t, task)
	if pack.Discovery.Withheld == 0 {
		t.Error("the withheld match must be counted, so the pack can say so")
	}
}

// A seed needs a task word in its own name: words only its path carries never
// add up to one.
func TestContextForTask_Discover_PathWordsNeverAddUpToASeed(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"task": "pricing discount rules"})
	for _, sd := range pack.Seeds {
		if sd.Name == "Lookup" || sd.Name == "Off" || sd.Name == "Apply" {
			t.Errorf("%s was seeded on path words alone: %v", sd.Selector, seedSelectors(pack))
		}
	}
}

// Named tokens that resolve to nothing do not crowd the search out: it still
// fills the slots left after resolution, and the junk is reported.
func TestContextForTask_Discover_UnresolvedNamesDoNotCrowdOutSearch(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"task": "Fix `Cart.Total`, which calls `fmt.Sprintf` and `strings.Join`, when the ledger records twice"})
	if len(pack.Seeds) < 2 || !slices.ContainsFunc(pack.Seeds, func(sd contextSeed) bool { return sd.Origin == originFound }) {
		t.Errorf("search did not fill the slots the junk left: seeds %v, misses %+v", seedSelectors(pack), pack.Misses)
	}
	if len(pack.Misses) != 2 {
		t.Errorf("misses = %+v, want the two unresolved names", pack.Misses)
	}
}

// A declaration named in the task and found by search is one seed, not two.
func TestContextForTask_Discover_NoDuplicateSeeds(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"task": "`Cart.Total` cart total"})
	n := 0
	for _, sd := range pack.Seeds {
		if sd.Path == "cart/cart.go" && sd.Name == "Total" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("(*Cart).Total seeded %d times: %v", n, seedSelectors(pack))
	}
}

// Proposals are absolute, so they resolve on a contested pin, which refuses the
// relative paths a caller never gave. Control: a relative explicit seed is
// refused there.
func TestContextForTask_Discover_ResolvesOnAContestedPin(t *testing.T) {
	s := newShop(t)
	s.collector.WithContested(func() bool { return true })
	if _, err := s.run(t, map[string]any{"files": []string{"cart/cart.go"}}); err == nil {
		t.Fatal("control: a relative explicit seed must be refused on a contested pin")
	}
	pack := s.collect(t, map[string]any{"task": "the checkout charges the ledger; see pricing/discount.go"})
	if len(pack.Seeds) < 2 || len(pack.Misses) != 0 {
		t.Errorf("seeds %v, misses %+v: proposals must resolve on a contested pin", seedSelectors(pack), pack.Misses)
	}
}

// The pack names the real reason it has no seed, and the path-shaped tokens that
// are no file here.
func TestContextForTask_Discover_SaysWhyNothingWasSeeded(t *testing.T) {
	s := newShop(t)
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"task": "cart total", "within": []string{"docs/**"}}, "outside within/corpora left out"},
		{map[string]any{"task": "`Nope.Missing` only"}, "no proposal resolved to one declaration"},
		{map[string]any{"task": "see pricing/nothere.go"}, "named in the task but no file here: pricing/nothere.go"},
	}
	for _, c := range cases {
		out, err := s.run(t, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%v: pack does not say %q:\n%s", c.args, c.want, out)
		}
	}
}

// A cancelled call is an error, never a quiet empty pack.
func TestContextForTask_Discover_CancelledCallIsAnError(t *testing.T) {
	s := newShop(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, _ := json.Marshal(map[string]any{"task": "the checkout charges the ledger"})
	if out, err := s.tool.Execute(ctx, raw); err == nil {
		t.Errorf("a cancelled call returned a pack:\n%s", out)
	}
}

// Equal hits order by path, then line; one hit per declaration is kept.
func TestRankDiscovered_TieBreakAndOnePerDeclaration(t *testing.T) {
	n := func(name, p string, line int) topology.SearchResult {
		return topology.SearchResult{Node: topology.Node{Name: name, Qualified: name, Path: p, StartLine: line, Kind: topology.KindFunction}}
	}
	hits := []topology.SearchResult{n("Total", "b/b.go", 1), n("Total", "a/a.go", 9), n("Total", "a/a.go", 9), n("Total", "a/a.go", 2)}
	got, _ := rankDiscovered(hits, []string{"total"}, contextScope{root: "/r"})
	order := make([]string, 0, len(got))
	for _, h := range got {
		order = append(order, fmt.Sprintf("%s:%d", h.node.Path, h.node.StartLine))
	}
	if !slices.Equal(order, []string{"a/a.go:2", "a/a.go:9", "b/b.go:1"}) {
		t.Errorf("order = %v", order)
	}
}

func TestIsFileUnder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.go"), []byte("package f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		s    string
		want bool
	}{{"f.go", true}, {filepath.Join(root, "f.go"), true}, {"dir.go", false}, {"../f.go", false}, {"nope.go", false}} {
		if got := isFileUnder(root, c.s); got != c.want {
			t.Errorf("isFileUnder(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// Named code counts toward the 3-seed cap like everything else discovery
// proposes; what is over it is reported, not dropped.
func TestContextForTask_Discover_NamedCodeIsCappedToo(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"task": "`Cart.Total` `Cart.Add` `Cart.Remove` `Ledger.Record`"})
	if len(pack.Seeds) != contextMaxDiscovered {
		t.Errorf("seeds %v, want the cap %d", seedSelectors(pack), contextMaxDiscovered)
	}
	if !slices.ContainsFunc(pack.Misses, func(m contextMiss) bool {
		return m.Input == "Ledger.Record" && strings.Contains(m.Reason, "over the 3-seed cap for proposals")
	}) {
		t.Errorf("the fourth named selector is not reported over the cap: %+v", pack.Misses)
	}
}

// One ambiguous selector is tried once, however many declarations share it, and
// a miss names it root-relative.
func TestContextForTask_Discover_AnAmbiguousSelectorIsOneMiss(t *testing.T) {
	root := copyShopFixture(t)
	src := "def setup_ledger():\n    pass\n\n\ndef setup_ledger():\n    pass\n"
	if err := os.WriteFile(filepath.Join(root, "scripts", "twice.py"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newShopTool(t, openContextStore(t, root, shopFixtureFiles+1), root)
	pack := s.collect(t, map[string]any{"task": "setup ledger"})
	n := 0
	for _, m := range pack.Misses {
		if strings.Contains(m.Input, "setup_ledger") {
			n++
			if strings.HasPrefix(m.Input, "/") {
				t.Errorf("miss input %q is absolute; the pack speaks root-relative", m.Input)
			}
		}
	}
	if n != 1 {
		t.Errorf("setup_ledger produced %d misses, want 1: %+v", n, pack.Misses)
	}
}

// A seed needs the minimum score as well as a name hit: one name word in a test
// file scores 1 after the demotion and does not seed.
func TestDiscoveredHit_Qualifies(t *testing.T) {
	hit := func(name, p string) topology.SearchResult {
		return topology.SearchResult{Node: topology.Node{Name: name, Qualified: name, Path: p, Kind: topology.KindFunction}}
	}
	got, _ := rankDiscovered([]topology.SearchResult{hit("Ledger", "s/s.go"), hit("Ledger", "s/s_test.go")}, []string{"ledger"}, contextScope{root: "/r"})
	if len(got) != 2 || !got[0].qualifies() || got[1].qualifies() {
		t.Errorf("production %+v should qualify and its test twin (score 1) should not", got)
	}
}
