package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_expand_test.go — the fixture cases measured against the frozen gold in
// testdata/contextpack/cases.json (PLAN-462 A3): expansion, evidence, ranking,
// gap candidates, type members, freshness and language coverage. The seeds, the
// intent and the task prose of each case are read from the gold file, so a case
// cannot drift from its oracle.

// oracleCase is one case of the gold file.
type oracleCase struct {
	ID     string `json:"id"`
	Intent string `json:"intent"`
	Task   string `json:"task"`
	Seeds  struct {
		Files   []string `json:"files"`
		Symbols []struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"symbols"`
	} `json:"seeds"`
	Gold map[string]json.RawMessage `json:"gold"`
}

func oracle(t *testing.T, id string) oracleCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "contextpack", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []oracleCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("cases.json has no case %s", id)
	return oracleCase{}
}

// args is the request the case describes.
func (c oracleCase) args() map[string]any {
	a := map[string]any{"intent": c.Intent}
	if len(c.Seeds.Files) > 0 {
		a["files"] = c.Seeds.Files
	}
	var symbols []string
	for _, s := range c.Seeds.Symbols {
		if s.Path != "" {
			symbols = append(symbols, s.Path+"#"+s.Name)
		} else {
			symbols = append(symbols, s.Name)
		}
	}
	if len(symbols) > 0 {
		a["symbols"] = symbols
	}
	if c.Task != "" {
		a["task"] = c.Task
	}
	return a
}

// gold lists a gold key's items as "path::selector", less any trailing note.
func (c oracleCase) gold(t *testing.T, key string) []string {
	t.Helper()
	raw, ok := c.Gold[key]
	if !ok {
		return nil
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("%s gold.%s: %v", c.ID, key, err)
	}
	for i := range items {
		items[i], _, _ = strings.Cut(items[i], " (")
	}
	return items
}

func goldID(path, selector string) string { return path + "::" + selector }

func (p *contextPack) related(id string) (contextRelated, bool) {
	for _, r := range p.Related {
		if goldID(r.Node.Path, nodeSelector(r.Node)) == id {
			return r, true
		}
	}
	return contextRelated{}, false
}

// bodyHash is the content_sha256 of the body delivered for id, a seed or a related
// declaration, or "" when none was sliced.
func (p *contextPack) bodyHash(id string) string {
	for i, s := range p.Seeds {
		if s.Kind == seedSymbol && goldID(s.Path, s.Selector) == id && i < len(p.Bodies) && p.Bodies[i].State == bodyReady {
			return p.Bodies[i].SHA
		}
	}
	if r, ok := p.related(id); ok && r.Body.State == bodyReady {
		return r.Body.SHA
	}
	return ""
}

func relatedIDs(p contextPack) []string {
	ids := make([]string, 0, len(p.Related))
	for _, r := range p.Related {
		ids = append(ids, goldID(r.Node.Path, nodeSelector(r.Node)))
	}
	return ids
}

// checkGold asserts the pack carries every gold item of c: a body as a delivered
// body with its printed hash, related and member items as ranked lines, a gap
// candidate as a labelled candidate and never a ranked relationship.
func checkGold(t *testing.T, c oracleCase, p contextPack, out string) {
	t.Helper()
	for _, id := range c.gold(t, "bodies") {
		sha := p.bodyHash(id)
		if sha == "" || !strings.Contains(out, "content_sha256="+sha) {
			t.Errorf("%s: gold body %s was not delivered as a body:\n%s", c.ID, id, out)
		}
	}
	for _, key := range []string{"related", "members"} {
		for _, id := range c.gold(t, key) {
			r, ok := p.related(id)
			if !ok || r.Gap || !strings.Contains(out, relatedText(r)) {
				t.Errorf("%s: gold %s %s is not a ranked relationship line (found=%v gap=%v)", c.ID, key, id, ok, r.Gap)
			}
		}
	}
	for _, id := range c.gold(t, "gap_candidates") {
		r, ok := p.related(id)
		if !ok || !r.Gap || r.Evidence != evidenceGap || !strings.Contains(out, gapText(r)) {
			t.Errorf("%s: gold gap candidate %s is not a labelled evidence-0 candidate (found=%v gap=%v)", c.ID, id, ok, r.Gap)
		}
	}
	for _, id := range c.gold(t, "related_intrafile") {
		r, ok := p.related(id)
		if !ok || r.Evidence != evidenceHeuristic || r.Source != sourceHeuristic {
			t.Errorf("%s: gold intra-file relation %s is not a heuristic edge (found=%v %+v)", c.ID, id, ok, r)
		}
	}
}

// requireNeverNoCallers fails if the output states that nothing calls anything.
// "Not proof of no callers" is the sanctioned disclaimer; a bare "no callers" is a
// claim the call graph cannot support.
func requireNeverNoCallers(t *testing.T, out string) {
	t.Helper()
	if cleaned := strings.ReplaceAll(strings.ToLower(out), "not proof of no callers", ""); strings.Contains(cleaned, "no callers") {
		t.Errorf("the pack claims there are no callers:\n%s", out)
	}
}

func runCase(t *testing.T, s shopTool, c oracleCase) (contextPack, string) {
	t.Helper()
	pack := s.collect(t, c.args())
	out, err := s.run(t, c.args())
	if err != nil {
		t.Fatalf("%s: %v", c.ID, err)
	}
	return pack, out
}

// C01: a method seed. Its callees are derived edges (evidence 2), shown with the
// source and confidence they carry, and the pack says the graph is Go-only,
// syntactic and blind to receiver calls instead of listing no callers.
func TestContextForTask_C01_CalleesAreDerivedEvidenceAndTheGraphIsDisclosed(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C01")
	pack, out := runCase(t, s, c)
	checkGold(t, c, pack, out)
	for _, id := range c.gold(t, "related") {
		r, _ := pack.related(id)
		if r.Evidence != evidenceDerived || r.Source != sourceCallResolver || r.Dist != 1 || !strings.HasPrefix(r.Via, "callee of ") {
			t.Errorf("%s: %+v, want a derived call-resolver callee one hop from the seed", id, r)
		}
		if !strings.Contains(out, "[e2 derived] "+r.Node.Name+" ") || !strings.Contains(out, "(call-resolver 0.9)") {
			t.Errorf("%s is not rendered with its evidence class and source:\n%s", id, out)
		}
	}
	if !strings.Contains(out, labelGoCallGraph) || !strings.Contains(out, labelNoProof) {
		t.Errorf("the Go-only, syntactic call graph is not disclosed:\n%s", out)
	}
	requireNeverNoCallers(t, out)
	if !strings.Contains(out, "callers of (*Cart).Total: none resolved in the index") {
		t.Errorf("an empty caller list is not worded as an absence of resolution:\n%s", out)
	}
	for _, r := range pack.Related {
		if r.Node.Path == "store/ledger.go" {
			t.Errorf("forbidden: the same-named Ledger.Total reached the pack: %+v", r)
		}
	}
}

// C02: a file seed and prose. The file's top declarations start the walk, the
// callees and the same-file declarations they lead to are the residual, and the
// prose only re-ranks: the same set with or without it.
func TestContextForTask_C02_ProseReRanksAndNeverSeeds(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C02")
	pack, out := runCase(t, s, c)
	checkGold(t, c, pack, out)

	args := c.args()
	delete(args, "task")
	bare := s.collect(t, args)
	withIDs, bareIDs := relatedIDs(pack), relatedIDs(bare)
	if !slices.Equal(slices.Sorted(slices.Values(withIDs)), slices.Sorted(slices.Values(bareIDs))) {
		t.Errorf("prose changed WHICH nodes are in the pack:\n with: %v\n bare: %v", withIDs, bareIDs)
	}
	code, add := goldID("cart/cart.go", "(*Cart).ApplyCode"), goldID("cart/cart.go", "(*Cart).Add")
	if slices.Index(withIDs, code) >= slices.Index(withIDs, add) || slices.Index(bareIDs, code) <= slices.Index(bareIDs, add) {
		t.Errorf("prose about discount codes should lift ApplyCode over Add, and only with the prose:\n with: %v\n bare: %v", withIDs, bareIDs)
	}
	r, _ := pack.related(code)
	if rb, _ := bare.related(code); r.Q <= 0 || rb.Q != 0 || r.Score <= rb.Score {
		t.Errorf("Q with prose = %v (score %v), without = %v (score %v)", r.Q, r.Score, rb.Q, rb.Score)
	}
	// The residual (outside the seed file) arrives by edges and structure, with its
	// evidence class: callees are derived, the file-mates heuristic.
	for id, want := range map[string]int{
		goldID("pricing/discount.go", "Lookup"):   evidenceDerived,
		goldID("pricing/discount.go", "Apply"):    evidenceDerived,
		goldID("pricing/discount.go", "Discount"): evidenceHeuristic,
		goldID("pricing/discount.go", "codes"):    evidenceHeuristic,
	} {
		if r, ok := pack.related(id); !ok || r.Evidence != want {
			t.Errorf("%s evidence = %d (found=%v), want %d", id, r.Evidence, ok, want)
		}
	}
	if !strings.Contains(out, "next:") || strings.Contains(out, "unconfirmed") {
		t.Errorf("a file seed with no unresolved caller question should not list gap candidates:\n%s", out)
	}
}

// C05: a type seed lists its members as structure, attributes no call edge to the
// type, and says a struct's fields are not graph members.
func TestContextForTask_C05_TypeMembersAreListedAndNoCallEdgeIsFabricated(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C05")
	pack, out := runCase(t, s, c)
	checkGold(t, c, pack, out)
	for _, id := range c.gold(t, "members") {
		r, _ := pack.related(id)
		if r.Evidence != evidenceExtractor || r.Via != "member of Cart" {
			t.Errorf("%s: evidence %d via %q, want an extractor member of Cart", id, r.Evidence, r.Via)
		}
	}
	for _, r := range pack.Related {
		if r.CallerOf != 0 || strings.HasPrefix(r.Via, "callee of Cart") || strings.HasPrefix(r.Via, "caller of Cart") {
			t.Errorf("forbidden: a call edge attributed to the type itself: %+v", r)
		}
	}
	if !strings.Contains(out, labelMembers) {
		t.Errorf("the pack does not say struct fields are not graph members:\n%s", out)
	}
	if strings.Contains(out, "callers of Cart") {
		t.Errorf("a type was given a caller list:\n%s", out)
	}
}

// C06: a function seed whose only route to Checkout is a receiver call. The
// resolved caller is an edge (evidence 2); Checkout is reached through the import
// graph, surfaced as an evidence-0 gap candidate that says it is not a resolved
// caller, and the pack never concludes there are no callers.
func TestContextForTask_C06_ReverseImportGapCandidatesAreLabelledNotCallers(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C06")
	pack, out := runCase(t, s, c)
	checkGold(t, c, pack, out)

	total, _ := pack.related(goldID("cart/cart.go", "(*Cart).Total"))
	if total.Evidence != evidenceDerived || !strings.HasPrefix(total.Via, "caller of Apply") || total.CallerOf == 0 {
		t.Errorf("Total: %+v, want a derived resolved caller of Apply", total)
	}
	checkout, _ := pack.related(goldID("api/checkout.go", "Checkout"))
	if !checkout.Gap || checkout.CallerOf != 0 || checkout.Dist != 2 || !strings.Contains(checkout.Via, "imports package cart, home of (*Cart).Total") {
		t.Errorf("Checkout: %+v, want a gap candidate two hops out, via the import of cart", checkout)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Checkout") && (strings.HasPrefix(line, "  [e") || strings.Contains(line, "caller of")) {
			t.Errorf("Checkout is presented as a relationship or a caller: %q", line)
		}
	}
	for _, want := range []string{gapCandidateHeading, "unresolved receiver calls possible — not a resolved caller", labelNoProof} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	requireNeverNoCallers(t, out)

	// Control: where nothing imports the seed's package there is no candidate to
	// list, and the pack still does not call the empty list proof.
	reports := s.collect(t, map[string]any{"symbols": []string{"reports/reports.go#Summarise"}})
	for _, r := range reports.Related {
		if r.Gap {
			t.Errorf("a package nothing imports produced a gap candidate: %+v", r)
		}
	}
}

// Importers that already hold a resolved call into the seed are not a gap, and a
// candidate is a declaration of an importing file, not the file's tests.
func TestContextForTask_GapCandidatesExcludeResolvedCallersAndTests(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"symbols": []string{"pricing/discount.go#Apply"}, "intent": contextIntentChange})
	for _, r := range pack.Related {
		if r.Gap && (r.Node.Path == "cart/cart.go" || isTestPath(r.Node.Path)) {
			t.Errorf("%s is a resolved caller's file or a test, so it is not a gap candidate", r.Node.Path)
		}
	}
	if _, ok := pack.related(goldID("api/checkout.go", "Checkout")); !ok {
		t.Fatal("control: Checkout, an importer of cart with no resolved call, is missing")
	}
}

// C10: a language with no cross-file call graph. The intra-file edge is heuristic
// evidence (1) and the pack says the cross-file graph is unavailable; it does not
// borrow Go's disclosure, and it does not conclude there are no callers.
func TestContextForTask_C10_PythonRelationsAreHeuristicAndTheGapIsNamed(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C10")
	pack, out := runCase(t, s, c)
	checkGold(t, c, pack, out)
	r, _ := pack.related(goldID("scripts/export.py", "format_row"))
	if r.Evidence != evidenceHeuristic || r.Source != sourceHeuristic || r.Conf != 0.8 {
		t.Errorf("format_row: %+v, want a heuristic edge at 0.8", r)
	}
	if !strings.Contains(out, "[e1 heuristic] format_row") {
		t.Errorf("the heuristic class is not rendered:\n%s", out)
	}
	if want := "cross-file call graph unavailable for python; intra-file heuristic call edges only (confidence 0.8)"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, labelGoCallGraph) {
		t.Errorf("a Python-only pack borrowed the Go call-graph disclosure:\n%s", out)
	}
	for _, r := range pack.Related {
		if r.Gap {
			t.Errorf("a language with no import-resolved graph produced a gap candidate: %+v", r)
		}
	}
	requireNeverNoCallers(t, out)
}

// C14 and Status: a seeded file the index cannot parse is a coverage gap at the
// seed and, from the index census, across the workspace; neither reads as "no
// symbols".
func TestContextForTask_C14_UncoveredLanguageIsACoverageGapNotAbsence(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C14")
	pack, out := runCase(t, s, c)
	if len(pack.Related) != 0 {
		t.Errorf("an uncovered file seed produced relations: %v", relatedIDs(pack))
	}
	for _, want := range []string{"coverage gap: svelte is not covered by an extractor", "coverage gap: the index holds files it has no extractor for (svelte (1))"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "no symbols") {
		t.Errorf("an uncovered file was described as having no symbols:\n%s", out)
	}
	// Control: a covered file seed carries no census line.
	covered, err := s.run(t, map[string]any{"files": []string{"cart/cart.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(covered, "the index holds files it has no extractor for") {
		t.Errorf("a covered seed was given the uncovered census:\n%s", covered)
	}
}

// C09: a relationship read from a file that changed since it was indexed says so,
// on the relationship and in the gaps; an unchanged file carries no such label.
func TestContextForTask_C09_RelationshipsFromAChangedFileAreLabelledPossiblyStale(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C09")
	file := filepath.Join(s.root, "cart", "cart.go")

	_, out := runCase(t, s, c)
	if strings.Contains(out, "possibly stale") || strings.Contains(out, labelStale) {
		t.Fatalf("control: an unchanged file was labelled stale:\n%s", out)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	inserted := "// Count returns how many lines the cart holds.\nfunc (c *Cart) Count() int { return len(c.items) }\n\n// Total returns"
	if err := os.WriteFile(file, []byte(strings.Replace(string(data), "// Total returns", inserted, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	pack, out := runCase(t, s, c)
	for _, id := range []string{goldID("pricing/discount.go", "Apply"), goldID("pricing/discount.go", "Lookup")} {
		r, ok := pack.related(id)
		if !ok || !r.Stale || !strings.Contains(relatedText(r), "possibly stale") {
			t.Errorf("%s, reached from the changed file, is not labelled possibly stale (found=%v stale=%v)", id, ok, r.Stale)
		}
	}
	if !strings.Contains(out, labelStale) {
		t.Errorf("output lacks %q:\n%s", labelStale, out)
	}
	// The relation reached from an UNCHANGED file is not marked, so the label means
	// something: Lookup's file-mates were reached from pricing, which did not change.
	if r, ok := pack.related(goldID("pricing/discount.go", "Discount")); ok && r.Stale {
		t.Errorf("a relation from an unchanged file was marked stale: %+v", r)
	}
}

// failIndex puts the index into the failing state and returns the function that
// heals it. A file untouched by the test is rewritten so the indexing cycle has
// something to persist, and the persist fails.
func failIndex(t *testing.T, s shopTool, store *topology.Store) (heal func()) {
	t.Helper()
	heal = injectPersistFault(t, s.root)
	path := filepath.Join(s.root, "store", "ledger.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n// touched\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	store.Enqueue("store/ledger.go")
	waitForHealth(t, store, "failing", func(h topology.Health) bool { return h.Failing })
	return heal
}

// C09b: while the index is failing, or absent, the pack makes no caller, callee or
// test-impact claim at all. The seed's body still comes from a disk snapshot, and
// the pack says why the rest is missing. Positive control: the same call on the
// healthy index has a neighbourhood; and recovery restores it.
func TestContextForTask_C09b_AFailingIndexSupportsNoRelationshipClaim(t *testing.T) {
	s := newShop(t)
	store := s.collector.store()
	c := oracle(t, "C09b")

	healthy, _ := runCase(t, s, c)
	if len(healthy.Related) == 0 {
		t.Fatal("control: the healthy index produced no neighbourhood")
	}

	heal := failIndex(t, s, store)
	pack, out := runCase(t, s, c)
	if len(pack.Related) != 0 || pack.Expansion.Ran {
		t.Errorf("a failing index still produced a neighbourhood: %v", relatedIDs(pack))
	}
	for _, want := range []string{labelIndexFailing, staleIndexMarker} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, claim := range []string{"[e", "callers of", "gap candidates", labelGoCallGraph, "callee of", "caller of"} {
		if strings.Contains(out, claim) {
			t.Errorf("a failing index still produced the claim %q:\n%s", claim, out)
		}
	}
	for _, id := range c.gold(t, "bodies") {
		if sha := pack.bodyHash(id); sha == "" || !strings.Contains(out, "content_sha256="+sha) {
			t.Errorf("gold body %s was not delivered from the disk snapshot:\n%s", id, out)
		}
	}

	heal()
	store.Enqueue("store/ledger.go")
	waitForHealth(t, store, "recovered", func(h topology.Health) bool { return !h.Failing && h.State == "idle" })
	recovered, out := runCase(t, s, c)
	if len(recovered.Related) == 0 || strings.Contains(out, labelIndexFailing) {
		t.Errorf("recovery did not restore the neighbourhood and drop the label:\n%s", out)
	}
}

// An absent index is the other half of C09b: the file seed still resolves from
// disk, nothing relational is claimed, and the label says so.
func TestContextForTask_C09b_AnAbsentIndexClaimsNothingRelational(t *testing.T) {
	root := copyShopFixture(t)
	col := NewContextCollector(nil).WithWorkspace(func(context.Context) string { return root }).WithBoundary(testBoundaryGuard(root))
	raw, _ := json.Marshal(map[string]any{"files": []string{"cart/cart.go"}, "intent": contextIntentChange})
	out, err := NewContextForTask(col).Execute(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, labelIndexFailing) || strings.Contains(out, "[e") || strings.Contains(out, "gap candidates") {
		t.Errorf("an absent index must be labelled and must claim nothing relational:\n%s", out)
	}
}
