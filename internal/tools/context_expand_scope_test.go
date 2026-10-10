package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/sqlitex"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_expand_scope_test.go — what bounds the walk: the scope filter at every
// hop (Invariant 2), the depth, node and time caps, deterministic order, the
// call-graph admission gate and the withholding of sensitive bodies the walk
// reaches (PLAN-462 A3).

// newModuleWorkspace writes and indexes a Go module. go.mod is recorded by the
// indexer without symbols, so it is not among the files waited for.
func newModuleWorkspace(t *testing.T, files map[string]string) shopTool {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, files)
	n := 0
	for name := range files {
		if name != "go.mod" {
			n++
		}
	}
	return newShopTool(t, openContextStore(t, root, n), root)
}

const chainModule = "module example.com/x\n\ngo 1.22\n"

// A public seed calls a function outside `within`, which calls one inside it. The
// outside neighbour is dropped when it is reached and nothing is expanded through
// it: if the filter ran only when the pack is rendered, the walk would pass
// through mid/b.go and the inside function would be found.
func TestContextForTask_ScopeIsAppliedAtEveryHopNotOnlyAtRender(t *testing.T) {
	s := newModuleWorkspace(t, map[string]string{
		"go.mod":    chainModule,
		"pub/a.go":  "package pub\n\nimport \"example.com/x/mid\"\n\n// A starts the chain.\nfunc A() int { return mid.B() }\n",
		"mid/b.go":  "package mid\n\nimport \"example.com/x/tail\"\n\n// B is outside the scope.\nfunc B() int { return tail.C() }\n",
		"tail/c.go": "package tail\n\n// C is inside the scope but reachable only through B.\nfunc C() int { return 1 }\n",
	})
	seed := map[string]any{"symbols": []string{"pub/a.go#A"}}

	open := s.collect(t, seed)
	if got, want := relatedIDs(open), []string{goldID("mid/b.go", "B"), goldID("tail/c.go", "C")}; !slices.Equal(got, want) {
		t.Fatalf("control: with no within the walk reached %v, want %v", got, want)
	}

	narrow := map[string]any{"symbols": []string{"pub/a.go#A"}, "within": []string{"pub", "tail"}}
	pack := s.collect(t, narrow)
	if len(pack.Related) != 0 {
		t.Errorf("the walk went through a node outside within: %v", relatedIDs(pack))
	}
	if pack.Expansion.Stats.Excluded != 1 {
		t.Errorf("excluded = %d, want the one file the walk met and refused", pack.Expansion.Stats.Excluded)
	}
	out, err := s.run(t, narrow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "mid/b.go") || strings.Contains(out, "tail/c.go") {
		t.Errorf("a path outside within, or one reachable only through it, reached the response:\n%s", out)
	}
	if !strings.Contains(out, "1 file(s) reached from the seeds are outside within/corpora and were left out; nothing was expanded through them") {
		t.Errorf("the exclusion is not disclosed by count:\n%s", out)
	}
}

// The same rule governs the reverse-import route: an importer outside the scope is
// never listed as a candidate.
func TestContextForTask_GapCandidatesObeyTheScopeToo(t *testing.T) {
	s := newShop(t)
	args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}}
	if pack := s.collect(t, args); len(pack.Related) == 0 {
		t.Fatal("control: no neighbourhood")
	} else if _, ok := pack.related(goldID("api/checkout.go", "Checkout")); !ok {
		t.Fatalf("control: Checkout is not a candidate without within: %v", relatedIDs(pack))
	}
	args["within"] = []string{"cart", "pricing"}
	pack := s.collect(t, args)
	for _, r := range pack.Related {
		if strings.HasPrefix(r.Node.Path, "api/") {
			t.Errorf("an importer outside within was listed: %+v", r)
		}
	}
	if pack.Expansion.Stats.Excluded == 0 {
		t.Error("the refused importer was not counted")
	}
}

// allowed is the one place a candidate is admitted: references, documents, plumb's
// own state and anything outside the scope are refused.
func TestExpander_AllowedAppliesScopeCorpusAndPlumbState(t *testing.T) {
	root := t.TempDir()
	scope, err := newContextScope(root, []string{"cart"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	docsOnly, err := newContextScope(root, nil, []string{corpusDocs})
	if err != nil {
		t.Fatal(err)
	}
	fn := func(path string) topology.Node { return topology.Node{Kind: topology.KindFunction, Path: path} }
	for _, tc := range []struct {
		name  string
		scope contextScope
		n     topology.Node
		want  bool
	}{
		{"inside within", scope, fn("cart/a.go"), true},
		{"outside within", scope, fn("pricing/a.go"), false},
		{"plumb's own state, with nothing else to exclude it", mustScope(t, root), fn(".plumb/hooks/a.go"), false},
		{"plumb's memories are the memory corpus, not code", mustScope(t, root), fn(".plumb/memories/a.go"), false},
		{"a name that merely starts like it", mustScope(t, root), fn(".plumbing/a.go"), true},
		{"an import is a reference", scope, topology.Node{Kind: topology.KindImport, Path: "cart/a.go"}, false},
		{"a package clause is a reference", scope, topology.Node{Kind: topology.KindPackage, Path: "cart/a.go"}, false},
		{"a document section", scope, topology.Node{Kind: topology.KindSection, Path: "cart/a.md"}, false},
		{"code is outside a docs-only corpus", docsOnly, fn("cart/a.go"), false},
		{"no narrowing admits any path in the root", mustScope(t, root), fn("x/y.go"), true},
	} {
		e := newExpander(nil, tc.scope, ranker{}, nil)
		if got := e.allowed(tc.n); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func mustScope(t *testing.T, root string) contextScope {
	t.Helper()
	s, err := newContextScope(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Depth is capped at two hops: a chain of four reaches the second function from
// the seed and not the third.
func TestContextForTask_DepthIsCappedAtTwoHops(t *testing.T) {
	s := newModuleWorkspace(t, map[string]string{
		"go.mod":  chainModule,
		"p0/a.go": "package p0\n\nimport \"example.com/x/p1\"\n\nfunc A() int { return p1.B() }\n",
		"p1/b.go": "package p1\n\nimport \"example.com/x/p2\"\n\nfunc B() int { return p2.C() }\n",
		"p2/c.go": "package p2\n\nimport \"example.com/x/p3\"\n\nfunc C() int { return p3.D() }\n",
		"p3/d.go": "package p3\n\nfunc D() int { return 1 }\n",
	})
	pack := s.collect(t, map[string]any{"symbols": []string{"p0/a.go#A"}})
	if got, want := relatedIDs(pack), []string{goldID("p1/b.go", "B"), goldID("p2/c.go", "C")}; !slices.Equal(got, want) {
		t.Errorf("related = %v, want exactly two hops: %v", got, want)
	}
	for _, r := range pack.Related {
		if r.Dist > contextExpansionDepth {
			t.Errorf("%s is %d hops out, past the cap of %d", r.Node.Path, r.Dist, contextExpansionDepth)
		}
	}
	if nodes, _ := s.store.ResolveNodes(t.Context(), "D", topology.NodeHint{}); len(nodes) == 0 {
		t.Fatal("control: D is not in the index, so its absence proves nothing")
	}
}

// hubFiles is a function that calls eighty others, each in a package of its own,
// all equally related to the seed.
func hubFiles(callees int) map[string]string {
	files := map[string]string{"go.mod": chainModule}
	var imports, calls strings.Builder
	for i := range callees {
		files[fmt.Sprintf("c%02d/f.go", i)] = fmt.Sprintf("package c%02d\n\nfunc F() int { return %d }\n", i, i)
		fmt.Fprintf(&imports, "\t\"example.com/x/c%02d\"\n", i)
		fmt.Fprintf(&calls, "\tn += c%02d.F()\n", i)
	}
	files["hub/hub.go"] = "package hub\n\nimport (\n" + imports.String() + ")\n\n// Run calls everything.\nfunc Run() int {\n\tn := 0\n" + calls.String() + "\treturn n\n}\n"
	return files
}

// The node cap is 60. Eighty equally ranked neighbours are cut to sixty by the
// frozen tie-break (path, then selector), the same sixty every time, and the cut is
// disclosed with its count.
func TestContextForTask_NodeCapIsSixtyAndTheCutIsDeterministic(t *testing.T) {
	s := newModuleWorkspace(t, hubFiles(80))
	args := map[string]any{"symbols": []string{"hub/hub.go#Run"}}
	first := s.collect(t, args)
	if len(first.Related) != contextMaxExpansionNodes {
		t.Fatalf("%d related nodes, want exactly the cap of %d", len(first.Related), contextMaxExpansionNodes)
	}
	want := make([]string, 0, contextMaxExpansionNodes)
	for i := range contextMaxExpansionNodes {
		want = append(want, goldID(fmt.Sprintf("c%02d/f.go", i), "F"))
	}
	if got := relatedIDs(first); !slices.Equal(got, want) {
		t.Errorf("the kept sixty are not the first sixty by path:\n got %v\nwant %v", got, want)
	}
	attempted := 0
	for _, r := range first.Related {
		if r.Body.State != bodyNone {
			attempted++
		}
	}
	if attempted != contextRelatedBodies {
		t.Errorf("%d related bodies were attempted, want the best-ranked %d and no more", attempted, contextRelatedBodies)
	}
	if first.Expansion.Stats.Truncated != 20 {
		t.Errorf("truncated = %d, want the 20 candidates beyond the cap", first.Expansion.Stats.Truncated)
	}
	for range 5 {
		if again := s.collect(t, args); !slices.Equal(relatedIDs(again), want) {
			t.Fatalf("the walk is not deterministic:\n got %v\nwant %v", relatedIDs(again), want)
		}
	}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "20 further candidate(s) beyond the 60-node, depth-2 cap were not kept") {
		t.Errorf("the cut is not disclosed:\n%s", out)
	}
	if !strings.Contains(out, "callers of Run: none resolved in the index") {
		t.Errorf("a capped walk must not speak of callers as if it were whole:\n%s", out)
	}
}

// One hop never fetches more than the traversal's own ceilings allow, so the walk
// cannot silently be clamped smaller than it was sized (PLAN-407).
func TestContextTraversalOpts_StayInsideTopologysCeilings(t *testing.T) {
	for _, derived := range []bool{false, true} {
		for _, typeCentre := range []bool{false, true} {
			opts := contextTraversalOpts(derived, expansionEdgeKinds(typeCentre))
			if got := topology.ClampTraversalOpts(opts); !reflect.DeepEqual(got, opts) {
				t.Errorf("derived=%v type=%v: the traversal clamps %+v to %+v", derived, typeCentre, opts, got)
			}
			if opts.Depth != 1 || opts.MaxNodes != contextHopNodes {
				t.Errorf("one hop at a time is what keeps the scope filter per-hop, got %+v", opts)
			}
		}
	}
	if slices.Contains(expansionEdgeKinds(true), "calls") || !slices.Contains(expansionEdgeKinds(false), "calls") {
		t.Error("a type must not be walked along call edges; a function must")
	}
}

// The deadline is a wall-clock cap on the walk: when it passes the pack is partial,
// says so, and still carries the seeds and their bodies.
func TestContextForTask_TheDeadlineYieldsAPartialPackThatSaysSo(t *testing.T) {
	s := newShop(t)
	args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}}
	if out, err := s.run(t, args); err != nil || strings.Contains(out, "deadline") {
		t.Fatalf("control: the default deadline cut the walk (err=%v):\n%s", err, out)
	}
	s.collector.deadline = time.Nanosecond
	pack := s.collect(t, args)
	if !pack.Expansion.Stats.Deadline || len(pack.Related) != 0 {
		t.Errorf("stats %+v, related %v: want a deadline stop with nothing walked", pack.Expansion.Stats, relatedIDs(pack))
	}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "expansion stopped at the 2s deadline: the related list is partial") {
		t.Errorf("a late walk is not disclosed:\n%s", out)
	}
	if len(parseBodies(t, out)) != 1 {
		t.Errorf("the seed's body must survive a deadline:\n%s", out)
	}
}

// insertEdge writes an edge straight into the index, the way the resolvers do.
func insertEdge(t *testing.T, root string, from, to topology.Node, source string, conf float64) {
	t.Helper()
	db, err := sqlitex.Open(topology.DBPath(root), sqlitex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO topology_edges(from_id, to_id, kind, confidence, source, to_identity) VALUES (?, ?, 'calls', ?, ?, '')`,
		from.ID, to.ID, conf, source); err != nil {
		t.Fatal(err)
	}
}

func resolveOne(t *testing.T, store *topology.Store, name string) topology.Node {
	t.Helper()
	nodes, err := store.ResolveNodes(t.Context(), name, topology.NodeHint{})
	if err != nil || len(nodes) == 0 {
		t.Fatalf("resolve %s: %v %v", name, nodes, err)
	}
	return nodes[0]
}

// Derived call edges are used only around a subject whose language the call-graph
// admission rule accepts. A derived edge recorded around a Python node is not
// followed; the same kind of edge around a Go node is. The control in each half
// proves the other half can fail.
func TestContextForTask_DerivedEdgesAreFollowedOnlyWhereTheCallGraphIsAdmitted(t *testing.T) {
	s := newShop(t)
	store := s.store
	formatRow, exportCharges := resolveOne(t, store, "format_row"), resolveOne(t, store, "export_charges")
	apply, lookup := resolveOne(t, store, "Apply"), resolveOne(t, store, "Lookup")
	insertEdge(t, s.root, formatRow, exportCharges, sourceCallResolver, 0.9) // python: not admitted
	insertEdge(t, s.root, apply, lookup, sourceCallResolver, 0.9)            // go: admitted

	py := s.collect(t, map[string]any{"symbols": []string{"scripts/export.py#format_row"}})
	for _, r := range py.Related {
		if r.Source == sourceCallResolver || r.Evidence == evidenceDerived {
			t.Errorf("a derived edge around a non-admitted language was followed: %+v", r)
		}
	}
	if r, ok := py.related(goldID("scripts/export.py", "export_charges")); !ok || r.Evidence != evidenceHeuristic || !strings.HasPrefix(r.Via, "caller of ") {
		t.Errorf("control: the extractor-side heuristic caller edge should still show, got %+v (found=%v)", r, ok)
	}

	goPack := s.collect(t, map[string]any{"symbols": []string{"pricing/discount.go#Apply"}})
	r, ok := goPack.related(goldID("pricing/discount.go", "Lookup"))
	if !ok || r.Evidence != evidenceDerived || r.Source != sourceCallResolver || !strings.HasPrefix(r.Via, "callee of Apply") {
		t.Errorf("control: the derived edge around an admitted Go node was not followed, got %+v (found=%v)", r, ok)
	}
}

// ---- Sensitive expansion (seam 8) ----

const sensitiveMarker = "SENSITIVE-FIXTURE-MARKER-9d41"

// newSensitiveFixture copies and indexes testdata/contextpack/sensitive, whose
// vault/secrets.go matches the default sensitive globs and is reachable from
// app/app.go only by a call edge.
func newSensitiveFixture(t *testing.T) shopTool {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(filepath.Join("testdata", "contextpack", "sensitive"))); err != nil {
		t.Fatal(err)
	}
	return newShopTool(t, openContextStore(t, root, 3), root)
}

// defaultSensitive is the daemon's rule with the default globs: the history
// store's IsSensitiveChange, which changeSensitive wraps. It records what it was
// asked.
type defaultSensitive struct {
	mu    sync.Mutex
	root  string
	asked [][2]string
}

func (d *defaultSensitive) fn(_ context.Context, path, from string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, [2]string{path, from})
	return history.IsSensitiveChange(config.DefaultHistorySensitiveGlobs(), d.root, path, from)
}

// A body the walk reaches in a sensitive file is a location and a label, never its
// content, wherever the content could leak: the line, the signature, the doc, a
// body, a read record. The marker is in all of them in the file. Controls: the same
// call without the decision delivers the marker (so the walk does reach it), and
// naming the file as a seed asks for it.
func TestContextForTask_ABodyReachedByExpansionInASensitiveFileIsLocationOnly(t *testing.T) {
	s := newSensitiveFixture(t)
	rule := &defaultSensitive{root: s.root}
	var recorded []string
	tracker := NewReadTracker()
	tracker.SetPersistSink(func(path string, _ time.Time, _ string) { recorded = append(recorded, path) })
	s.tool.WithReads(tracker)
	args := map[string]any{"symbols": []string{"app/app.go#Authorise"}}

	// Control: with no decision wired the walk reaches the secret and delivers it.
	open, err := s.run(t, args)
	if err != nil || !strings.Contains(open, sensitiveMarker) {
		t.Fatalf("control: the marker is not reachable by expansion (err=%v):\n%s", err, open)
	}

	s.collector.WithSensitive(rule.fn)
	recorded = nil
	pack := s.collect(t, args)
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, sensitiveMarker) {
		t.Fatalf("sensitive content reached the response:\n%s", out)
	}
	token, ok := pack.related(goldID("vault/secrets.go", "Token"))
	if !ok || !token.Withheld || token.Node.Signature != "" || token.Node.Docstring != "" || token.Body.State != bodyNone {
		t.Errorf("Token: %+v (found=%v), want a withheld location with no signature, doc or body", token, ok)
	}
	if line := relatedText(token); !strings.Contains(line, "vault/secrets.go:5") || !strings.Contains(line, "location only: sensitive path, content withheld") {
		t.Errorf("the location-only line is %q", line)
	}
	if len(parseRelatedBodies(t, out)) != 0 {
		t.Errorf("a body was delivered for a withheld declaration:\n%s", out)
	}
	if len(parseBodies(t, out)) != 1 {
		t.Errorf("the seed's own body must still be delivered:\n%s", out)
	}
	if len(recorded) != 1 || !strings.HasSuffix(recorded[0], "/app/app.go") {
		t.Errorf("recorded reads = %v, want only app/app.go, which was delivered", recorded)
	}
	canon := canonicalRoot(s.root)
	if !slices.Contains(rule.asked, [2]string{filepath.Join(canon, "vault", "secrets.go"), ""}) {
		t.Errorf("the decision was not asked about the absolute path with no copy source: %v", rule.asked)
	}

	// A file seed chooses nothing: its declarations are the tool's pick, so a
	// sensitive file seed is location-only too.
	fileSeed, err := s.run(t, map[string]any{"files": []string{"vault/secrets.go"}})
	if err != nil || strings.Contains(fileSeed, sensitiveMarker) || !strings.Contains(fileSeed, "location only") {
		t.Errorf("a sensitive file seed leaked or was not labelled (err=%v):\n%s", err, fileSeed)
	}

	// Naming a symbol is asking for it: an explicit seed is not expansion-acquired.
	named, err := s.run(t, map[string]any{"symbols": []string{"vault/secrets.go#Token"}})
	if err != nil || !strings.Contains(named, sensitiveMarker) {
		t.Errorf("an explicitly named seed should be delivered (err=%v):\n%s", err, named)
	}
}

// The line itself is a second guard: a withheld node handed to the renderer with
// its signature and doc still intact shows its location and nothing else.
func TestRelatedText_AWithheldNodeShowsNoSignatureOrDoc(t *testing.T) {
	r := contextRelated{
		Node:     topology.Node{Kind: topology.KindFunction, Name: "Token", Path: "vault/secrets.go", StartLine: 5, Signature: "func Token() string // " + sensitiveMarker, Docstring: sensitiveMarker},
		Evidence: evidenceDerived, Via: "callee of Authorise", Withheld: true,
	}
	if line := relatedText(r); strings.Contains(line, sensitiveMarker) || strings.Contains(line, "func Token") || !strings.Contains(line, "location only") {
		t.Errorf("a withheld node's line leaks or is unlabelled: %q", line)
	}
	r.Withheld = false
	if line := relatedText(r); !strings.Contains(line, "func Token") {
		t.Errorf("control: an ordinary node's line should carry its signature: %q", line)
	}
}

// A gap candidate is a declaration of a production file that imports the package:
// the importer's tests are not listed, and neither is a file with no declaration to
// name.
func TestContextForTask_GapCandidatesAreProductionDeclarationsOnly(t *testing.T) {
	s := newModuleWorkspace(t, map[string]string{
		"go.mod":            chainModule,
		"lib/lib.go":        "package lib\n\n// T is a thing.\ntype T struct{}\n\n// M does a thing.\nfunc (t *T) M() {}\n",
		"user/user.go":      "package user\n\nimport \"example.com/x/lib\"\n\n// Use calls M through a receiver.\nfunc Use(t *lib.T) { t.M() }\n",
		"user/user_test.go": "package user\n\nimport (\n\t\"testing\"\n\n\t\"example.com/x/lib\"\n)\n\nfunc TestUse(t *testing.T) { Use(&lib.T{}) }\n\n// helper is a plain function in a test file, which is still a test file's.\nfunc helper() {}\n",
	})
	pack := s.collect(t, map[string]any{"symbols": []string{"lib/lib.go#T.M"}})
	var gaps []string
	for _, r := range pack.Related {
		if r.Gap {
			gaps = append(gaps, goldID(r.Node.Path, nodeSelector(r.Node)))
		}
	}
	if !slices.Equal(gaps, []string{goldID("user/user.go", "Use")}) {
		t.Errorf("gap candidates = %v, want only the production importer's Use", gaps)
	}
}

// An importer that already holds a resolved call into the seed is not a gap: its
// remaining declarations (more than the same-file cap keeps as relations) are not
// listed as candidates. An importer with no resolved call is, which is the control.
func TestContextForTask_AnImporterWithAResolvedCallIsNotAGap(t *testing.T) {
	var extra strings.Builder
	for i := range 9 {
		fmt.Fprintf(&extra, "\nfunc Plain%d() {}\n", i)
	}
	s := newModuleWorkspace(t, map[string]string{
		"go.mod":         chainModule,
		"lib/lib.go":     "package lib\n\n// Do does a thing.\nfunc Do() {}\n",
		"user/user.go":   "package user\n\nimport \"example.com/x/lib\"\n\n// Calls resolves to Do.\nfunc Calls() { lib.Do() }\n" + extra.String(),
		"other/other.go": "package other\n\nimport \"example.com/x/lib\"\n\n// Use names Do without calling it.\nfunc Use() { _ = lib.Do }\n",
	})
	pack := s.collect(t, map[string]any{"symbols": []string{"lib/lib.go#Do"}})
	var gaps []string
	for _, r := range pack.Related {
		if r.Gap {
			gaps = append(gaps, goldID(r.Node.Path, nodeSelector(r.Node)))
		}
	}
	if !slices.Equal(gaps, []string{goldID("other/other.go", "Use")}) {
		t.Errorf("gap candidates = %v, want only the importer with no resolved call", gaps)
	}
	if _, ok := pack.related(goldID("user/user.go", "Calls")); !ok {
		t.Fatalf("control: the resolved caller is missing: %v", relatedIDs(pack))
	}
}

// The scrub comes before the score: a withheld node's doc cannot influence its
// rank, and a node that is not withheld keeps its signature and doc.
func TestExpander_AWithheldNodeIsScrubbedBeforeItIsScored(t *testing.T) {
	node := topology.Node{ID: 1, Kind: topology.KindFunction, Name: "Zzz", Path: "x/y.go", Signature: "func Zzz()", Docstring: "five dollar code"}
	rk := newRanker("five dollar code", contextIntentChange, nil)
	from := hopNode{node: topology.Node{Path: "a.go"}, dist: 0, sel: "A"}

	open := newExpander(nil, contextScope{}, rk, nil).makeRelated(node, from, evidenceDerived, "", 0, "callee of A")
	held := newExpander(nil, contextScope{}, rk, func(string) bool { return true }).makeRelated(node, from, evidenceDerived, "", 0, "callee of A")
	if open.Withheld || open.Q == 0 || open.Node.Signature == "" {
		t.Errorf("control: an unrestricted node was scrubbed or unscored: %+v", open)
	}
	if !held.Withheld || held.Node.Signature != "" || held.Node.Docstring != "" || held.Q != 0 {
		t.Errorf("a withheld node kept something of the file: %+v", held)
	}
}
