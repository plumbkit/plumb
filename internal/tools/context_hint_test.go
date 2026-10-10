package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_hint_test.go — (*ContextCollector).Hint and NewContextHinter (PLAN-462
// A4): selectors and locations only, no bodies, no reads, gaps instead of guesses,
// scope and sensitivity at every hop, bounded by bytes and by the deadline, keyed
// by the request's root.

var _ ContextHinter = NewContextHinter(nil, nil) // the factory a daemon calls returns the contract

// shopHinter is the hint side of the shop fixture, built the way the daemon would
// build it: from the root-keyed accessor and the sensitive decision, and nothing
// else (no workspace accessor, no boundary guard, no tracker).
func shopHinter(s shopTool, sensitive SensitivePathFn) ContextHinter {
	return NewContextHinter(storeForRoot(s.store), sensitive)
}

func hintReq(s shopTool, seeds ...ContextSeed) HintRequest {
	return HintRequest{Workspace: s.root, Agent: "test", Seeds: seeds, MaxBytes: 1 << 20}
}

func mustHint(t *testing.T, h ContextHinter, req HintRequest) HintResult {
	t.Helper()
	res, err := h.Hint(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// hintText is every string a result carries.
func hintText(res HintResult) string {
	parts := make([]string, 0, 1+len(res.Gaps)+4*len(res.Lines))
	parts = append(parts, res.Freshness)
	parts = append(parts, res.Gaps...)
	for _, l := range res.Lines {
		parts = append(parts, l.Selector, l.Path, l.Kind, l.Provenance)
	}
	return strings.Join(parts, "\n")
}

func hintSize(res HintResult) int {
	n := len(res.Freshness)
	for _, g := range res.Gaps {
		n += len(g)
	}
	for _, l := range res.Lines {
		n += hintLineBytes(l)
	}
	return n
}

func hintLine(res HintResult, selector string) (HintLine, bool) {
	for _, l := range res.Lines {
		if l.Selector == selector {
			return l, true
		}
	}
	return HintLine{}, false
}

// A hint is where the declarations near a seed live and why: the seed first, then
// its neighbourhood, each with the evidence class that reached it. Never source.
func TestContextHint_ListsSelectorsLocationsAndProvenanceNeverSource(t *testing.T) {
	s := newShop(t)
	res := mustHint(t, shopHinter(s, nil), hintReq(s, ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"}))

	if len(res.Lines) == 0 || res.Lines[0] != (HintLine{Selector: "(*Cart).Total", Path: "cart/cart.go", Kind: "method", Provenance: "seed", Line: 45}) {
		t.Fatalf("first line = %+v, want the seed's selector, location and kind", res.Lines)
	}
	apply, ok := hintLine(res, "Apply")
	if !ok || apply.Path != "pricing/discount.go" || apply.Line != 19 || apply.Kind != "function" || apply.Provenance != "e2 derived: callee of (*Cart).Total" {
		t.Errorf("Apply = %+v (found %v), want a derived callee with its location", apply, ok)
	}
	if res.Freshness != "fresh" || len(res.Gaps) != 0 || res.Omitted != 0 {
		t.Errorf("freshness %q gaps %q omitted %d, want a fresh, complete answer", res.Freshness, res.Gaps, res.Omitted)
	}
	// Index text only: nothing from a function's body, doc or signature.
	all := hintText(res)
	for _, source := range []string{"sub := 0", "pricing.Apply(sub", "Total returns the payable amount", "func (c *Cart)", shopRoot(s)} {
		if strings.Contains(all, source) {
			t.Errorf("the hint carries %q, which is source, documentation or an absolute path", source)
		}
	}
}

func shopRoot(s shopTool) string { return canonicalRoot(s.root) }

// Hint reads no file and records no read. The files are deleted after indexing, so a
// hint that opened one would fail or differ. The read record is the calling agent's
// own tracker, the very one a pack for that agent writes to (per-agent resolver, the
// agent carried by the context the hint is asked under), and it is compared whole
// before and after. Positive control: the same agent's pack, asked while the files
// exist, changes that tracker, so the comparison can tell.
func TestContextHint_ReadsNoFileAndRecordsNoRead(t *testing.T) {
	s := newShop(t)
	trackers := newAgentTrackers("a")
	var persisted atomic.Int32
	trackers["a"].SetPersistSink(func(string, time.Time, string) { persisted.Add(1) })
	s.tool.WithReadsFor(trackers.resolve)
	agent := asAgent("a")
	cart := filepath.Join(s.root, "cart", "cart.go")
	records := func() []ReadRecord {
		r := trackers["a"].Records()
		slices.SortFunc(r, func(a, b ReadRecord) int { return strings.Compare(a.Path, b.Path) })
		return r
	}

	if _, err := s.runAs(t, agent, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}}); err != nil {
		t.Fatal(err)
	}
	if persisted.Load() == 0 || trackers["a"].Mtime(cart).IsZero() {
		t.Fatal("control: a delivered body left the agent's tracker unchanged, so comparing it proves nothing")
	}
	before, persistedBefore := records(), persisted.Load()
	want := mustHint(t, shopHinter(s, nil), hintReq(s, ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"}))

	for _, f := range []string{"cart/cart.go", "pricing/discount.go", "docs/pricing.md"} {
		if err := os.Remove(filepath.Join(s.root, filepath.FromSlash(f))); err != nil {
			t.Fatal(err)
		}
	}
	// The collector is the one the tool gathers packs with, asked as the same agent.
	got, err := s.collector.Hint(agent, hintReq(s, ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Lines, want.Lines) || len(got.Lines) < 2 {
		t.Errorf("the hint changed when the files were deleted, so it read them:\n got %+v\nwant %+v", got.Lines, want.Lines)
	}
	if after := records(); !slices.Equal(after, before) || persisted.Load() != persistedBefore {
		t.Errorf("a hint changed the agent's read record:\n before %+v\n after  %+v", before, after)
	}
}

// The collector cannot record a read, by construction: it holds no read tracker and no
// way to reach one. This is what makes "a hint records nothing" true rather than
// merely observed, so a field added later that could record fails here. Positive
// control: the tool, which does record, is caught by the same check.
func TestContextCollector_HoldsNothingThatCouldRecordARead(t *testing.T) {
	recorders := func(typ reflect.Type) (offending []string) {
		for i := range typ.NumField() {
			if f := typ.Field(i); canRecordReads(f.Type) {
				offending = append(offending, f.Name)
			}
		}
		return offending
	}
	if got := recorders(reflect.TypeFor[ContextForTask]()); !slices.Equal(got, []string{"tracker", "readsFor"}) {
		t.Fatalf("control: the checker found %v in the tool, want its tracker and its per-agent resolver", got)
	}
	if got := recorders(reflect.TypeFor[ContextCollector]()); len(got) != 0 {
		t.Errorf("the collector holds something that could record a read: %v", got)
	}
}

// canRecordReads reports whether a value of typ is, or can hand back, a ReadTracker.
func canRecordReads(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Func:
		for i := range typ.NumIn() {
			if canRecordReads(typ.In(i)) {
				return true
			}
		}
		for i := range typ.NumOut() {
			if canRecordReads(typ.Out(i)) {
				return true
			}
		}
		return false
	case reflect.Interface:
		return typ.Implements(reflect.TypeFor[readRecorder]())
	}
	return typ == reflect.TypeFor[*ReadTracker]()
}

// readRecorder is anything with the tracker's recording method.
type readRecorder interface {
	Record(path string, mtime time.Time, sha string)
}

// An ambiguous or unresolved selector is a Gap and never a list of candidates: no
// line, and nothing of a candidate in any gap. Positive control: the same selector
// with its path resolves to a line.
func TestContextHint_AnAmbiguousOrUnresolvedSelectorIsAGapNeverCandidateLines(t *testing.T) {
	s := newShop(t)
	h := shopHinter(s, nil)
	for _, tc := range []struct {
		seed ContextSeed
		gap  string
	}{
		{ContextSeed{Symbol: "Total"}, `"Total": 2 declarations match; none was chosen`},
		{ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Coupon"}, `"cart/cart.go#Cart.Coupon": no indexed declaration matches; nothing was invented`},
		{ContextSeed{Path: "api/checkout.go", Symbol: "Cart.Add"}, `"api/checkout.go#Cart.Add": no declaration matches within api/checkout.go; the selector exists elsewhere in the index`},
	} {
		res := mustHint(t, h, hintReq(s, tc.seed))
		if len(res.Lines) != 0 {
			t.Errorf("%+v: candidate lines were offered: %+v", tc.seed, res.Lines)
		}
		if !slices.Contains(res.Gaps, tc.gap) {
			t.Errorf("%+v: gaps %q lack %q", tc.seed, res.Gaps, tc.gap)
		}
		for _, candidate := range []string{"(*Ledger).Total", "store/ledger.go", "(*Cart).Add", "cart/cart.go:20"} {
			if strings.Contains(hintText(res), candidate) && !strings.Contains(tc.gap, candidate) {
				t.Errorf("%+v: the result names the candidate %q", tc.seed, candidate)
			}
		}
	}
	if res := mustHint(t, h, hintReq(s, ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Add"})); len(res.Lines) == 0 || res.Lines[0].Selector != "(*Cart).Add" {
		t.Errorf("control: an exact selector did not resolve: %+v", res)
	}
}

// Code corpus only, inside the root, never plumb's own state; each refusal is a gap
// that names only what the caller wrote.
func TestContextHint_OnlyCodeInsideTheRootIsLookedUp(t *testing.T) {
	s := newShop(t)
	outside := filepath.Join(filepath.Dir(s.root), "elsewhere", "x.go")
	h := shopHinter(s, nil)
	for _, tc := range []struct {
		seed ContextSeed
		gap  string
	}{
		{ContextSeed{Path: "docs/pricing.md"}, "docs/pricing.md: hints cover code only (corpus docs is not in corpora)"},
		{ContextSeed{Path: ".plumb/memories/x.md"}, ".plumb/memories/x.md: plumb's own state"},
		{ContextSeed{Path: "../x.go"}, "../x.go: outside this agent's workspace root, so it was not looked up"},
		{ContextSeed{Path: outside}, outside + ": outside this agent's workspace root, so it was not looked up"},
		{ContextSeed{Path: shopRoot(s) + "/cart/../api/checkout.go"}, shopRoot(s) + "/cart/../api/checkout.go: an absolute path with '..' is not looked up"},
		{ContextSeed{}, "a seed names neither a path nor a symbol"},
	} {
		res := mustHint(t, h, hintReq(s, tc.seed))
		// A gap is clamped to contextHintGapBytes, so an expected text and a gap agree
		// when one is a prefix of the other, up to the clamp's ellipsis.
		if len(res.Lines) != 0 || !slices.ContainsFunc(res.Gaps, func(g string) bool {
			cut := strings.TrimSuffix(g, "…")
			return strings.HasPrefix(g, tc.gap) || (len(cut) > 80 && strings.HasPrefix(tc.gap, cut))
		}) {
			t.Errorf("%+v: lines %+v gaps %q, want no lines and a gap starting %q", tc.seed, res.Lines, res.Gaps, tc.gap)
		}
	}
	if res := mustHint(t, h, hintReq(s, ContextSeed{Path: "web/CartBadge.svelte"})); len(res.Lines) != 0 ||
		!slices.ContainsFunc(res.Gaps, func(g string) bool { return strings.Contains(g, "svelte is not covered by an extractor") }) {
		t.Errorf("an uncovered language must be a coverage gap, got lines %+v gaps %q", res.Lines, res.Gaps)
	}
	// A path inside the root, spelled absolutely, is the same file.
	if res := mustHint(t, h, hintReq(s, ContextSeed{Path: shopRoot(s) + "/cart/cart.go", Symbol: "Cart.Add"})); len(res.Lines) == 0 || res.Lines[0].Path != "cart/cart.go" {
		t.Errorf("control: an absolute path inside the root was not looked up: %+v", res)
	}
}

// A sensitive path is named and never walked through: a sensitive seed has its line
// and no neighbourhood; a sensitive neighbour has its line, labelled, and nothing
// beyond it. Control: with no sensitive decision both neighbourhoods are there.
func TestContextHint_SensitivePathsAreNamedAndNotWalkedThrough(t *testing.T) {
	s := newShop(t)
	seed := ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"}
	open := mustHint(t, shopHinter(s, nil), hintReq(s, seed))
	for _, want := range []string{"Apply", "Lookup", "Discount", "codes"} {
		if _, ok := hintLine(open, want); !ok {
			t.Fatalf("control: the open neighbourhood lacks %s: %+v", want, open.Lines)
		}
	}
	sensitiveIn := func(rel string) SensitivePathFn {
		return func(_ context.Context, p, _ string) bool { return strings.HasSuffix(filepath.ToSlash(p), "/"+rel) }
	}

	seedSensitive := mustHint(t, shopHinter(s, sensitiveIn("cart/cart.go")), hintReq(s, seed))
	if len(seedSensitive.Lines) != 1 || seedSensitive.Lines[0].Provenance != "seed; location only: sensitive path" {
		t.Errorf("a sensitive seed must be named alone, labelled, got %+v", seedSensitive.Lines)
	}

	nbrSensitive := mustHint(t, shopHinter(s, sensitiveIn("pricing/discount.go")), hintReq(s, seed))
	apply, ok := hintLine(nbrSensitive, "Apply")
	if !ok || !strings.HasSuffix(apply.Provenance, "; location only: sensitive path") {
		t.Errorf("a sensitive neighbour must be named and labelled, got %+v (found %v)", apply, ok)
	}
	for _, beyond := range []string{"Discount", "codes"} {
		if _, ok := hintLine(nbrSensitive, beyond); ok {
			t.Errorf("%s was reached through a sensitive path", beyond)
		}
	}
}

// The result fits the budget however small, keeps the lines in rank order with the
// rest counted, and defaults to the per-turn budget when none is given.
func TestContextHint_IsBoundedByMaxBytesAndCountsWhatItDropped(t *testing.T) {
	s := newShop(t)
	h := shopHinter(s, nil)
	seed := ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"}
	full := mustHint(t, h, hintReq(s, seed))
	if len(full.Lines) < 4 {
		t.Fatalf("control: the unbounded hint has only %d lines", len(full.Lines))
	}
	sawPartial := false
	for budget := 1; budget <= hintSize(full)+8; budget++ {
		req := hintReq(s, seed)
		req.MaxBytes = budget
		res := mustHint(t, h, req)
		if hintSize(res) > budget {
			t.Fatalf("MaxBytes %d: the result is %d bytes", budget, hintSize(res))
		}
		if !slices.Equal(res.Lines, full.Lines[:len(res.Lines)]) {
			t.Fatalf("MaxBytes %d: the lines are not a prefix of the ranked list: %+v", budget, res.Lines)
		}
		if res.Omitted != len(full.Lines)-len(res.Lines) {
			t.Fatalf("MaxBytes %d: omitted %d, want %d", budget, res.Omitted, len(full.Lines)-len(res.Lines))
		}
		sawPartial = sawPartial || (res.Omitted > 0 && len(res.Lines) > 0)
	}
	if !sawPartial {
		t.Error("the sweep never produced a partial result, so it proved nothing about the cut")
	}
	req := hintReq(s, seed)
	req.MaxBytes = 0
	if res := mustHint(t, h, req); hintSize(res) > contextHintDefaultBytes || len(res.Lines) == 0 {
		t.Errorf("the default budget gave %d bytes and %d lines", hintSize(res), len(res.Lines))
	}
}

// A deadline that has passed is a partial answer with a gap, never a late answer:
// nothing is looked up, and the index is not even asked for. Positive control: with
// time left the same request is answered.
func TestContextHint_AnExpiredDeadlineIsAPartialWithAGapAndNoLookup(t *testing.T) {
	s := newShop(t)
	var asked atomic.Int32
	h := NewContextHinter(func(r string) *topology.Store { asked.Add(1); return storeForRoot(s.store)(r) }, nil)
	req := hintReq(s, ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"})

	req.Deadline = time.Now().Add(-time.Second)
	res := mustHint(t, h, req)
	if len(res.Lines) != 0 || !slices.Contains(res.Gaps, hintDeadlineGap) || asked.Load() != 0 {
		t.Errorf("expired deadline: lines %+v gaps %q index asked %d times; want no lines, the deadline gap, no lookup", res.Lines, res.Gaps, asked.Load())
	}

	req.Deadline = time.Now().Add(time.Minute)
	if res := mustHint(t, h, req); len(res.Lines) == 0 || slices.Contains(res.Gaps, hintDeadlineGap) || asked.Load() == 0 {
		t.Errorf("control: with time left want an answer, got %+v (asked %d)", res, asked.Load())
	}
}

// A deadline that passes while the index is being reached is also a partial: the
// answer returns promptly after it, without lines taken after the clock ran out.
// Positive control: the same slow accessor, given a generous deadline, is waited for
// and the hint is complete, so the partial above is the deadline's doing and not a
// hint that never could have answered.
func TestContextHint_ADeadlinePassingMidwayIsAPartialNotALateAnswer(t *testing.T) {
	s := newShop(t)
	const slow = 150 * time.Millisecond
	h := NewContextHinter(func(r string) *topology.Store { time.Sleep(slow); return storeForRoot(s.store)(r) }, nil)
	req := hintReq(s, ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"})

	req.Deadline = time.Now().Add(50 * time.Millisecond)
	start := time.Now()
	res := mustHint(t, h, req)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the hint took %s after a 50ms deadline", elapsed)
	}
	if len(res.Lines) != 0 || !slices.Contains(res.Gaps, hintDeadlineGap) {
		t.Errorf("want no lines and the deadline gap, got %+v %q", res.Lines, res.Gaps)
	}

	req.Deadline = time.Now().Add(time.Minute)
	start = time.Now()
	full := mustHint(t, h, req)
	if elapsed := time.Since(start); elapsed < slow {
		t.Errorf("control: the slow accessor was not waited for (%s), so the partial above proves nothing", elapsed)
	}
	if len(full.Lines) < 2 || slices.Contains(full.Gaps, hintDeadlineGap) {
		t.Errorf("control: with time to spare the hint must be complete, got %+v %q", full.Lines, full.Gaps)
	}
}

// With a failing index the seeds are named and nothing is walked: no relationship is
// claimed, and the hint says the index is failing. With none at all, the same.
func TestContextHint_AFailingOrAbsentIndexClaimsNoRelationship(t *testing.T) {
	s := newShop(t)
	seed := ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"}
	heal := failIndex(t, s, s.store)
	res := mustHint(t, shopHinter(s, nil), hintReq(s, seed))
	heal()
	if len(res.Lines) != 1 || res.Lines[0].Provenance != "seed" || !slices.Contains(res.Gaps, labelIndexFailing) || res.Freshness != "stale" {
		t.Errorf("failing index: lines %+v gaps %q freshness %q; want only the seed, the failing label, stale", res.Lines, res.Gaps, res.Freshness)
	}

	none := mustHint(t, NewContextHinter(func(string) *topology.Store { return nil }, nil), hintReq(s, seed))
	if len(none.Lines) != 0 || none.Freshness != "unavailable" || !slices.ContainsFunc(none.Gaps, func(g string) bool { return strings.Contains(g, "no topology index is available for this root") }) {
		t.Errorf("absent index: %+v", none)
	}
}

// A file seed starts the walk at the file's own declarations; a file the index lists
// nothing for says so.
func TestContextHint_AFileSeedListsItsDeclarationsAndAnEmptyOneSaysSo(t *testing.T) {
	s := newShop(t)
	h := shopHinter(s, nil)
	res := mustHint(t, h, hintReq(s, ContextSeed{Path: "pricing/discount.go"}))
	apply, ok := hintLine(res, "Apply")
	if !ok || apply.Provenance != "e3 extractor: declared in seed file pricing/discount.go" {
		t.Errorf("Apply = %+v (found %v), want a declaration of the seed file", apply, ok)
	}
	empty := mustHint(t, h, hintReq(s, ContextSeed{Path: "scripts/nothing.py"}))
	if len(empty.Lines) != 0 || !slices.Contains(empty.Gaps, "scripts/nothing.py: the index lists no declaration to start from") {
		t.Errorf("a file the index lists nothing for: %+v", empty)
	}
}
