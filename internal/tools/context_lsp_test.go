package tools

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_lsp_test.go — the language-server refinement of the top symbol seeds
// (PLAN-462 A5, C08): a server that answers adds and confirms edges at evidence 4 and
// says so; one that is slow is cut at the sub-deadline; one that is absent, warming or
// failing leaves the structural pack as it was, labelled. None of them blocks the pack
// or fails it.

// fakeLSP is a scripted language server for the shop fixture. Every request first goes
// through gate, which is how a test makes it slow, hung or failing.
type fakeLSP struct {
	root     string
	gate     func(ctx context.Context) error // nil answers at once
	incoming []protocol.CallHierarchyItem    // the callers it reports for any item
	outgoing []protocol.CallHierarchyItem    // the callees it reports for any item
	noItem   bool                            // prepareCallHierarchy resolves nothing
	// per overrides incoming and outgoing for the declaration whose document symbol is
	// named so ("(*Cart).Add"), so one server can answer differently for each seed.
	per map[string]fakeAnswer

	mu    sync.Mutex
	calls []string
}

func (f *fakeLSP) record(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what)
}

func (f *fakeLSP) count(what string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == what {
			n++
		}
	}
	return n
}

func (f *fakeLSP) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLSP) enter(ctx context.Context, what string) error {
	f.record(what)
	if f.gate != nil {
		return f.gate(ctx)
	}
	return nil
}

// shopSymbols are gopls-shaped document symbols for the three shop files the tests
// ask about: methods flat and named with their receiver, ranges 0-based.
func shopSymbols(uri string) []protocol.DocumentSymbol {
	sym := func(name string, kind protocol.SymbolKind, startLine, endLine uint32) protocol.DocumentSymbol {
		return protocol.DocumentSymbol{
			Name: name, Kind: kind,
			Range:          protocol.Range{Start: protocol.Position{Line: startLine}, End: protocol.Position{Line: endLine}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: startLine, Character: 6}, End: protocol.Position{Line: startLine, Character: 10}},
		}
	}
	switch {
	case strings.HasSuffix(uri, "/cart/cart.go"):
		return []protocol.DocumentSymbol{
			sym("(*Cart).Add", protocol.SKMethod, 19, 27), sym("(*Cart).Remove", protocol.SKMethod, 30, 38),
			sym("(*Cart).Total", protocol.SKMethod, 44, 50),
		}
	case strings.HasSuffix(uri, "/pricing/discount.go"):
		return []protocol.DocumentSymbol{sym("Lookup", protocol.SKFunction, 15, 15), sym("Apply", protocol.SKFunction, 18, 20)}
	}
	return nil
}

func (f *fakeLSP) DocumentSymbols(ctx context.Context, p protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	if err := f.enter(ctx, "documentSymbol"); err != nil {
		return nil, err
	}
	return shopSymbols(p.TextDocument.URI), nil
}

// fakeAnswer is what a server says about one declaration.
type fakeAnswer struct{ incoming, outgoing []protocol.CallHierarchyItem }

func (f *fakeLSP) PrepareCallHierarchy(ctx context.Context, p protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	if err := f.enter(ctx, "prepareCallHierarchy"); err != nil {
		return nil, err
	}
	if f.noItem {
		return nil, nil
	}
	name := "seed"
	for _, s := range shopSymbols(p.TextDocument.URI) {
		if s.SelectionRange.Start.Line == p.Position.Line {
			name = s.Name
		}
	}
	return []protocol.CallHierarchyItem{{Name: name, Kind: protocol.SKMethod, URI: p.TextDocument.URI}}, nil
}

func (f *fakeLSP) IncomingCalls(ctx context.Context, p protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	if err := f.enter(ctx, "incomingCalls"); err != nil {
		return nil, err
	}
	list := f.incoming
	if a, ok := f.per[p.Item.Name]; ok {
		list = a.incoming
	}
	out := make([]protocol.CallHierarchyIncomingCall, 0, len(list))
	for _, it := range list {
		out = append(out, protocol.CallHierarchyIncomingCall{From: it})
	}
	return out, nil
}

func (f *fakeLSP) OutgoingCalls(ctx context.Context, p protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	if err := f.enter(ctx, "outgoingCalls"); err != nil {
		return nil, err
	}
	list := f.outgoing
	if a, ok := f.per[p.Item.Name]; ok {
		list = a.outgoing
	}
	out := make([]protocol.CallHierarchyOutgoingCall, 0, len(list))
	for _, it := range list {
		out = append(out, protocol.CallHierarchyOutgoingCall{To: it})
	}
	return out, nil
}

// item is a call-hierarchy item for the declaration at 1-based line of rel under root.
func (f *fakeLSP) item(rel, name string, line uint32) protocol.CallHierarchyItem {
	pos := protocol.Position{Line: line - 1}
	return protocol.CallHierarchyItem{
		Name: name, Kind: protocol.SKFunction, URI: toFileURI(filepath.Join(f.root, filepath.FromSlash(rel))),
		Range: protocol.Range{Start: pos, End: pos}, SelectionRange: protocol.Range{Start: pos, End: pos},
	}
}

// answeringLSP is a server that knows who calls Total (Checkout, which the syntactic
// graph cannot resolve: a receiver call) and what Total calls (Apply and Lookup, which
// the index already has).
func answeringLSP(s shopTool) *fakeLSP {
	f := &fakeLSP{root: canonicalRoot(s.root)}
	f.incoming = []protocol.CallHierarchyItem{f.item("api/checkout.go", "Checkout", 11)}
	f.outgoing = []protocol.CallHierarchyItem{f.item("pricing/discount.go", "Apply", 19), f.item("pricing/discount.go", "Lookup", 16)}
	return f
}

// withLSP wires the fake (and an optional warm-up probe) to the shop's collector.
func withLSP(s shopTool, f ContextLSP, warm LSPWarmupFn) shopTool {
	s.collector.WithLSP(f, warm)
	return s
}

var totalArgs = map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}}

// C08, the server answers: the edge it alone can see (a receiver-method caller) is
// added and the edges the index already drew are confirmed, all at evidence 4; the
// caller is no longer an unconfirmed candidate; LSP state is its own line. Positive
// control: with no server, the same caller is an e0 gap candidate and nothing is e4.
func TestContextForTask_LSP_AnAnsweringServerAddsAndConfirmsEdgesAtEvidence4(t *testing.T) {
	plain := newShop(t)
	base := plain.collect(t, totalArgs)
	if r, ok := base.related(goldID("api/checkout.go", "Checkout")); !ok || !r.Gap || r.Evidence != evidenceGap {
		t.Fatalf("control: without a server Checkout must be an e0 gap candidate, got %+v (found %v)", r, ok)
	}
	for _, r := range base.Related {
		if r.Evidence == evidenceLSP {
			t.Fatalf("control: %s is e4 with no server wired", r.Node.Path)
		}
	}

	s := newShop(t)
	f := answeringLSP(s)
	s = withLSP(s, f, nil)
	pack := s.collect(t, totalArgs)

	if l := pack.LSP; l.State != lspRefined || l.Asked != 1 || l.Answered != 1 || l.Confirmed != 2 || l.Added != 1 || l.Unplaced != 0 {
		t.Fatalf("LSP = %+v, want refined: 1 of 1 answered, 2 confirmed (Apply, Lookup), 1 added (Checkout)", l)
	}
	seed := pack.Seeds[0]
	checkout, ok := pack.related(goldID("api/checkout.go", "Checkout"))
	if !ok || checkout.Gap || checkout.Evidence != evidenceLSP || checkout.Source != sourceLSP || checkout.CallerOf != seed.ID {
		t.Errorf("Checkout = %+v (found %v), want a resolved e4 caller of the seed", checkout, ok)
	}
	for _, callee := range []string{"Apply", "Lookup"} {
		r, ok := pack.related(goldID("pricing/discount.go", callee))
		if !ok || r.Evidence != evidenceLSP || r.Source != sourceLSP {
			t.Errorf("%s = evidence %d source %q (found %v), want the index's callee confirmed at e4", callee, r.Evidence, r.Source, ok)
		}
	}
	// A node the server did not confirm keeps the evidence the index gave it.
	if r, ok := pack.related(goldID("pricing/discount.go", "Discount")); ok && r.Evidence == evidenceLSP {
		t.Errorf("Discount, which the server never mentioned, was raised to e4: %+v", r)
	}
	for _, call := range []string{"documentSymbol", "prepareCallHierarchy", "incomingCalls", "outgoingCalls"} {
		if n := f.count(call); n != 1 {
			t.Errorf("%s was asked %d times, want once for the one seed", call, n)
		}
	}

	out := renderContextPack(pack)
	for _, want := range []string{
		"lsp: language server consulted for 1 of 1 top callable symbol seed(s): 2 structural call edge(s) confirmed, 1 added (e4)",
		"related (", "e4 language-server edge, e3 extractor edge",
		"  [e4 lsp] Checkout — api/checkout.go:11 function; caller of (*Cart).Total (lsp 1.0)",
		"  [e4 lsp] Apply — pricing/discount.go:19 function; callee of (*Cart).Total (lsp 1.0)",
		"callers of (*Cart).Total: 1 resolved and listed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, gapCandidateHeading) || strings.Contains(out, labelStructural) {
		t.Errorf("Checkout is still a candidate, or the pack still says no server was consulted:\n%s", out)
	}
}

// A slow server is cut at the sub-deadline, the pack is labelled and returned inside
// the call's own deadline, and the structural pack is whole. Positive control: the
// same slow server, given a generous sub-deadline, finishes and refines.
func TestContextForTask_LSP_ASlowServerIsCutAtTheSubDeadline(t *testing.T) {
	const perRequest = 300 * time.Millisecond
	slow := func(ctx context.Context) error {
		select {
		case <-time.After(perRequest):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	t.Run("generous sub-deadline: the same server answers", func(t *testing.T) {
		s := newShop(t)
		f := answeringLSP(s)
		f.gate = slow
		s = withLSP(s, f, nil)
		s.collector.lspDeadline = 10 * time.Second
		start := time.Now()
		pack := s.collect(t, totalArgs)
		if elapsed := time.Since(start); elapsed < 3*perRequest {
			t.Fatalf("control: four %s requests took %s, so the server was not waited for", perRequest, elapsed)
		}
		if pack.LSP.State != lspRefined || pack.LSP.Added != 1 {
			t.Errorf("control: with time the slow server must refine, got %+v", pack.LSP)
		}
	})

	t.Run("default sub-deadline: cut, labelled, inside the total", func(t *testing.T) {
		s := newShop(t)
		f := answeringLSP(s)
		f.gate = slow
		s = withLSP(s, f, nil)
		start := time.Now()
		out, err := s.run(t, totalArgs)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("a slow server failed the pack: %v", err)
		}
		if elapsed < contextLSPSubDeadline*3/4 {
			t.Errorf("the call took %s: the server was not waited for up to the %s sub-deadline", elapsed, contextLSPSubDeadline)
		}
		if elapsed > 1500*time.Millisecond || elapsed >= contextExpansionDeadline {
			t.Errorf("the call took %s: the %s sub-deadline did not bound the wait inside the %s total", elapsed, contextLSPSubDeadline, contextExpansionDeadline)
		}
		assertStructuralPackKept(t, out)
		for _, want := range []string{"lsp: structural-only partial result; LSP enrichment unavailable: cut at the 800ms sub-deadline after "} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "[e4 ") || strings.Contains(out, "language server consulted for") {
			t.Errorf("a cut refinement presented e4 edges or a completed consultation:\n%s", out)
		}
	})
}

// assertStructuralPackKept checks the pack is the whole structural pack: the seed's
// complete body, its callee from the index, and the Go-only call graph disclosure.
func assertStructuralPackKept(t *testing.T, out string) {
	t.Helper()
	if bodies := parseBodies(t, out); len(bodies) != 1 || !strings.Contains(bodies[0].text, "func (c *Cart) Total()") {
		t.Errorf("the seed's body was lost:\n%s", out)
	}
	for _, want := range []string{"  [e2 derived] Apply — pricing/discount.go:19", labelGoCallGraph} {
		if !strings.Contains(out, want) {
			t.Errorf("the structural pack lost %q:\n%s", want, out)
		}
	}
}

// A server that ignores its context cannot hold the pack: the round trip is abandoned
// when the sub-deadline passes, whatever the client does. The hung call is released at
// the end of the test.
func TestContextForTask_LSP_AServerThatIgnoresItsContextCannotHoldThePack(t *testing.T) {
	s := newShop(t)
	f := answeringLSP(s)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// Hung, but not for ever: a pack that did wait would fail the bound below rather
	// than hang the suite.
	f.gate = func(context.Context) error {
		select {
		case <-release:
		case <-time.After(4 * time.Second):
		}
		return nil
	}
	s = withLSP(s, f, nil)

	start := time.Now()
	out, err := s.run(t, totalArgs)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("a hung server held the pack for %s", elapsed)
	}
	if f.total() == 0 {
		t.Fatal("control: the server was never asked, so nothing was hung")
	}
	assertStructuralPackKept(t, out)
	if !strings.Contains(out, "LSP enrichment unavailable: cut at the 800ms sub-deadline") {
		t.Errorf("the hung server is not reported as a cut:\n%s", out)
	}
}

// C08 as the gold file states it: with no server the pack is structural-only, says
// so in the required words, has the gold body and the gold related declaration, and
// does not wait. Every way the server can be absent gives the same phrase and its own
// cause.
func TestContextForTask_LSP_C08_AnAbsentServerLeavesAStructuralPackLabelled(t *testing.T) {
	c := oracle(t, "C08")
	for _, tc := range []struct {
		name  string
		wire  func(s shopTool) shopTool
		cause string
		asked bool // whether the server is expected to have been asked
	}{
		{"none wired", func(s shopTool) shopTool { return s }, "no language server is wired for this call", false},
		{"still warming", func(s shopTool) shopTool {
			return withLSP(s, answeringLSP(s), func(string) (bool, time.Duration) { return true, 4 * time.Second })
		}, "the language server is still warming (~4s elapsed), so it was not asked", false},
		{"errors", func(s shopTool) shopTool {
			f := answeringLSP(s)
			f.gate = func(context.Context) error { return errors.New("no language server is configured for go") }
			return withLSP(s, f, nil)
		}, "the language server did not answer: no language server is configured for go", true},
		{"has no call hierarchy", func(s shopTool) shopTool {
			f := answeringLSP(s)
			f.noItem = true
			return withLSP(s, f, nil)
		}, "the server resolved no call-hierarchy item for (*Cart).Total", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.wire(newShop(t))
			start := time.Now()
			out, err := s.run(t, c.args())
			if err != nil {
				t.Fatalf("an absent server failed the pack: %v", err)
			}
			if elapsed := time.Since(start); elapsed > contextLSPSubDeadline {
				t.Errorf("the call took %s: it waited as if for the %s sub-deadline", elapsed, contextLSPSubDeadline)
			}
			want := "lsp: structural-only partial result; LSP enrichment unavailable: " + tc.cause
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
			if !strings.Contains(out, "structural-only partial result; LSP enrichment unavailable") {
				t.Errorf("output lacks the gold file's required label:\n%s", out)
			}
			assertStructuralPackKept(t, out)
			pack := s.collect(t, c.args())
			for _, id := range c.gold(t, "related") {
				if r, ok := pack.related(id); !ok || r.Evidence == evidenceLSP {
					t.Errorf("gold related %s = %+v (found %v), want the index's relationship", id, r, ok)
				}
			}
			if strings.Contains(out, "waiting past the enrichment sub-deadline") {
				t.Errorf("the pack words a wait it should not have made:\n%s", out)
			}
		})
	}

	// Control: the label belongs to the absence. A server that answers does not carry it.
	s := newShop(t)
	s = withLSP(s, answeringLSP(s), nil)
	if out, err := s.run(t, c.args()); err != nil || strings.Contains(out, "LSP enrichment unavailable") {
		t.Errorf("control: an answering server still produced the unavailable label (err %v):\n%s", err, out)
	}
}

// A warming server is not asked at all (a cold server would spend the whole
// sub-deadline answering nothing), and a server asked about no seed is not asked.
func TestContextForTask_LSP_IsAskedOnlyWhenThereIsSomethingToAskAndSomeoneToAsk(t *testing.T) {
	s := newShop(t)
	f := answeringLSP(s)
	s = withLSP(s, f, func(string) (bool, time.Duration) { return true, 0 })
	s.collect(t, totalArgs)
	if f.total() != 0 {
		t.Errorf("a warming server was asked %d times", f.total())
	}

	for name, args := range map[string]map[string]any{
		"a file seed alone":     {"files": []string{"cart/cart.go"}},
		"a type seed alone":     {"symbols": []string{"cart/cart.go#Cart"}},
		"an unresolved symbol":  {"symbols": []string{"cart/cart.go#Cart.Missing"}},
		"a refused-scope seed":  {"symbols": []string{"cart/cart.go#Cart.Total"}, "within": []string{"docs"}},
		"corpora without code ": {"symbols": []string{"cart/cart.go#Cart.Total"}, "corpora": []string{"docs"}},
	} {
		s := newShop(t)
		f := answeringLSP(s)
		s = withLSP(s, f, nil)
		pack := s.collect(t, args)
		if f.total() != 0 || pack.LSP.State != lspOff {
			t.Errorf("%s: the server was asked %d times and LSP state is %v, want no question at all", name, f.total(), pack.LSP.State)
		}
	}
	// Control: the same server, asked about a callable seed, is asked.
	s = newShop(t)
	f = answeringLSP(s)
	s = withLSP(s, f, nil)
	s.collect(t, totalArgs)
	if f.total() == 0 {
		t.Fatal("control: a callable symbol seed never reached the server")
	}
}

// Only the first three callable symbol seeds, in the order named, go to the server.
func TestContextForTask_LSP_OnlyTheTopThreeCallableSeedsAreAsked(t *testing.T) {
	s := newShop(t)
	f := answeringLSP(s)
	s = withLSP(s, f, nil)
	pack := s.collect(t, map[string]any{"symbols": []string{
		"cart/cart.go#Cart.Add", "cart/cart.go#Cart.Remove", "cart/cart.go#Cart.Total", "pricing/discount.go#Apply", "pricing/discount.go#Lookup",
	}})
	if got := f.count("prepareCallHierarchy"); got != contextLSPSeeds || pack.LSP.Asked != contextLSPSeeds {
		t.Errorf("%d call-hierarchy requests and Asked %d for five callable seeds, want %d", got, pack.LSP.Asked, contextLSPSeeds)
	}
}

// A server edge the index cannot match to the same declaration is counted and left
// out, never attached to a guess; one outside the scope is dropped; a sensitive one is
// named by location only. Each has its control in the first test.
func TestContextForTask_LSP_EdgesAreMatchedToTheIndexOrCountedNotGuessed(t *testing.T) {
	t.Run("a name the index does not have there", func(t *testing.T) {
		s := newShop(t)
		f := answeringLSP(s)
		f.incoming = []protocol.CallHierarchyItem{f.item("api/checkout.go", "SomethingElse", 11), f.item("api/checkout.go", "Checkout", 11)}
		s = withLSP(s, f, nil)
		pack := s.collect(t, totalArgs)
		if pack.LSP.Unplaced != 1 || pack.LSP.Added != 1 {
			t.Errorf("LSP = %+v, want the mismatched name counted unplaced and the matching one added", pack.LSP)
		}
		out := renderContextPack(pack)
		if !strings.Contains(out, "1 server edge(s) could not be placed in the index (it may be out of date) and were left out") {
			t.Errorf("the unplaced edge is not disclosed:\n%s", out)
		}
		if strings.Contains(out, "SomethingElse") {
			t.Errorf("an unmatched server name reached the pack:\n%s", out)
		}
	})

	t.Run("the other direction does not confirm the index's edge", func(t *testing.T) {
		s := newShop(t)
		f := answeringLSP(s)
		// The index has Apply as a callee of Total and Lookup likewise; a server that
		// names them as callers (and Total's own callee list as empty) has said nothing
		// about those edges.
		f.incoming = []protocol.CallHierarchyItem{f.item("pricing/discount.go", "Apply", 19)}
		f.outgoing = nil
		s = withLSP(s, f, nil)
		pack := s.collect(t, totalArgs)
		if l := pack.LSP; l.Confirmed != 0 || l.Added != 0 {
			t.Errorf("LSP = %+v, want a caller report on a callee to confirm and add nothing", l)
		}
		if r, ok := pack.related(goldID("pricing/discount.go", "Apply")); !ok || r.Evidence != evidenceDerived {
			t.Errorf("Apply = %+v (found %v), want the index's e2 callee untouched", r, ok)
		}

		f.incoming = []protocol.CallHierarchyItem{f.item("api/checkout.go", "Checkout", 11)}
		f.outgoing = []protocol.CallHierarchyItem{f.item("pricing/discount.go", "Apply", 19)}
		if l := s.collect(t, totalArgs).LSP; l.Confirmed != 1 || l.Added != 1 {
			t.Errorf("control: the right directions must confirm Apply and add Checkout, got %+v", l)
		}

		// An answer about one seed confirms that seed's edges and no other's: the server
		// says Add calls Apply (the index does not think so) and says nothing of Total, whose
		// callee Apply is in the index, so Apply is not confirmed by Add's answer.
		two := map[string]any{"symbols": []string{"cart/cart.go#Cart.Add", "cart/cart.go#Cart.Total"}}
		applyItem := f.item("pricing/discount.go", "Apply", 19)
		f.incoming, f.outgoing = nil, nil
		f.per = map[string]fakeAnswer{"(*Cart).Add": {outgoing: []protocol.CallHierarchyItem{applyItem}}, "(*Cart).Total": {}}
		pack = s.collect(t, two)
		if r, ok := pack.related(goldID("pricing/discount.go", "Apply")); !ok || r.Evidence != evidenceDerived || pack.LSP.Confirmed != 0 {
			t.Errorf("another seed's answer confirmed Total's edge: %+v (found %v), LSP %+v", r, ok, pack.LSP)
		}
		f.per["(*Cart).Total"] = fakeAnswer{outgoing: []protocol.CallHierarchyItem{applyItem}}
		pack = s.collect(t, two)
		if r, ok := pack.related(goldID("pricing/discount.go", "Apply")); !ok || r.Evidence != evidenceLSP || pack.LSP.Confirmed != 1 {
			t.Errorf("control: Total's own answer must confirm its edge: %+v (found %v), LSP %+v", r, ok, pack.LSP)
		}
		f.per = nil

		// A caller of one seed is not confirmed by a report about another seed: the index
		// has Total as the caller of Apply; the server says Total calls Add.
		both := map[string]any{"symbols": []string{"pricing/discount.go#Apply", "cart/cart.go#Cart.Add"}}
		totalItem := f.item("cart/cart.go", "Total", 45)
		f.incoming, f.outgoing = nil, nil
		f.per = map[string]fakeAnswer{"Apply": {}, "(*Cart).Add": {incoming: []protocol.CallHierarchyItem{totalItem}}}
		pack = s.collect(t, both)
		if r, ok := pack.related(goldID("cart/cart.go", "(*Cart).Total")); !ok || r.Evidence != evidenceDerived || pack.LSP.Confirmed != 0 {
			t.Errorf("a caller report about Add confirmed Total as Apply's caller: %+v (found %v), LSP %+v", r, ok, pack.LSP)
		}
		f.per = map[string]fakeAnswer{"Apply": {incoming: []protocol.CallHierarchyItem{totalItem}}, "(*Cart).Add": {}}
		pack = s.collect(t, both)
		if r, ok := pack.related(goldID("cart/cart.go", "(*Cart).Total")); !ok || r.Evidence != evidenceLSP || pack.LSP.Confirmed != 1 {
			t.Errorf("control: Apply's own caller report must confirm Total: %+v (found %v), LSP %+v", r, ok, pack.LSP)
		}

		f.per = nil

		// The same the other way round: for the seed Apply the index has Total as a
		// CALLER. A server that lists Total among Apply's callees contradicts it and
		// confirms nothing; one that lists it among the callers confirms it.
		apply := map[string]any{"symbols": []string{"pricing/discount.go#Apply"}}
		total := f.item("cart/cart.go", "Total", 45)
		f.incoming, f.outgoing = nil, []protocol.CallHierarchyItem{total}
		pack = s.collect(t, apply)
		if r, ok := pack.related(goldID("cart/cart.go", "(*Cart).Total")); !ok || r.Evidence == evidenceLSP || pack.LSP.Confirmed != 0 {
			t.Errorf("a callee report on the index's caller confirmed it: %+v (found %v), LSP %+v", r, ok, pack.LSP)
		}
		f.incoming, f.outgoing = []protocol.CallHierarchyItem{total}, nil
		pack = s.collect(t, apply)
		if r, ok := pack.related(goldID("cart/cart.go", "(*Cart).Total")); !ok || r.Evidence != evidenceLSP || pack.LSP.Confirmed != 1 {
			t.Errorf("control: a caller report on the index's caller must confirm it: %+v (found %v), LSP %+v", r, ok, pack.LSP)
		}
	})

	t.Run("outside within", func(t *testing.T) {
		s := newShop(t)
		s = withLSP(s, answeringLSP(s), nil)
		args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}, "within": []string{"cart", "pricing"}}
		pack := s.collect(t, args)
		if _, ok := pack.related(goldID("api/checkout.go", "Checkout")); ok {
			t.Errorf("a server caller outside within was listed: %v", relatedIDs(pack))
		}
		if pack.LSP.Added != 0 || pack.Expansion.Stats.Excluded == 0 {
			t.Errorf("LSP %+v, excluded %d: the refused edge must be counted as left out", pack.LSP, pack.Expansion.Stats.Excluded)
		}
	})

	t.Run("a sensitive file", func(t *testing.T) {
		s := newShop(t)
		s = withLSP(s, answeringLSP(s), nil)
		s.collector.WithSensitive(func(_ context.Context, p, _ string) bool {
			return strings.HasSuffix(filepath.ToSlash(p), "/api/checkout.go")
		})
		pack := s.collect(t, totalArgs)
		checkout, ok := pack.related(goldID("api/checkout.go", "Checkout"))
		if !ok || !checkout.Withheld || checkout.Node.Signature != "" || checkout.Body.State != bodyNone {
			t.Fatalf("Checkout = %+v (found %v), want a withheld, location-only caller with no body", checkout, ok)
		}
		if out := renderContextPack(pack); strings.Contains(out, "l *store.Ledger") || !strings.Contains(out, "location only: sensitive path") {
			t.Errorf("a sensitive server edge leaked its signature, or is not labelled:\n%s", out)
		}
	})

	t.Run("a hub's callers are capped", func(t *testing.T) {
		s := newShop(t)
		f := answeringLSP(s)
		f.incoming = nil
		for range contextLSPEdges + 6 {
			f.incoming = append(f.incoming, f.item("api/checkout.go", "Checkout", 11))
		}
		s = withLSP(s, f, nil)
		pack := s.collect(t, totalArgs)
		if pack.LSP.More != 6 || pack.LSP.Added != 1 {
			t.Errorf("LSP = %+v, want 6 edges beyond the per-answer cap counted and the one distinct caller added once", pack.LSP)
		}
		if !strings.Contains(renderContextPack(pack), "6 further server edge(s) beyond 24 per answer were not taken") {
			t.Error("the cut is not disclosed")
		}
	})
}

// confirmsCall is the whole rule for what a server's call report confirms: a call edge,
// from the same seed, in the direction the server named, that is not already e4. Each
// row changes one thing from the row that is confirmed.
func TestContextRelated_ConfirmsCall(t *testing.T) {
	const seed = 7
	callee := contextRelated{Edge: topology.EdgeCalls, Evidence: evidenceDerived, ParentID: seed}
	caller := contextRelated{Edge: topology.EdgeCalls, Evidence: evidenceDerived, ParentID: seed, CallerOf: seed}
	for _, tc := range []struct {
		name   string
		r      contextRelated
		inward bool
		want   bool
	}{
		{"a callee the server calls a callee", callee, false, true},
		{"a caller the server calls a caller", caller, true, true},
		{"a callee the server calls a caller", callee, true, false},
		{"a caller the server calls a callee", caller, false, false},
		{"a callee of another seed", contextRelated{Edge: topology.EdgeCalls, ParentID: seed + 1}, false, false},
		{"a caller of another seed", contextRelated{Edge: topology.EdgeCalls, ParentID: seed, CallerOf: seed + 1}, true, false},
		{"a member, not a call (outward)", contextRelated{Edge: topology.EdgeContains, ParentID: seed}, false, false},
		{"a member, not a call (inward)", contextRelated{Edge: topology.EdgeContains, ParentID: seed, CallerOf: seed}, true, false},
		{"a same-file mate, which no edge reached", contextRelated{ParentID: seed}, false, false},
		{"already confirmed", contextRelated{Edge: topology.EdgeCalls, Evidence: evidenceLSP, ParentID: seed}, false, false},
	} {
		if got := tc.r.confirmsCall(seed, tc.inward); got != tc.want {
			t.Errorf("%s: confirmsCall = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Hint is topology only: a collector wired to a server never puts a question to it
// for a hint, and the hint carries no e4.
func TestContextHint_NeverConsultsTheLanguageServer(t *testing.T) {
	s := newShop(t)
	f := answeringLSP(s)
	s = withLSP(s, f, nil)
	res := mustHint(t, s.collector, hintReq(s, ContextSeed{Path: "cart/cart.go", Symbol: "Cart.Total"}))
	if f.total() != 0 {
		t.Errorf("a hint put %d questions to the language server", f.total())
	}
	if len(res.Lines) < 2 || slices.ContainsFunc(res.Lines, func(l HintLine) bool { return strings.HasPrefix(l.Provenance, "e4 ") }) {
		t.Errorf("hint lines = %+v, want the structural neighbourhood and nothing from a server", res.Lines)
	}
}

// The sub-deadline is the contract's 800 ms inside the 2 s total, three seeds: a
// constant a reviewer can see, pinned apart from anything that uses it.
func TestContextLSP_LimitsAreTheGatesLimits(t *testing.T) {
	if contextLSPSeeds != 3 || contextLSPSubDeadline != 800*time.Millisecond || contextLSPSubDeadline >= contextExpansionDeadline {
		t.Errorf("limits = %d seeds, %s sub-deadline inside %s; the gate says 3 seeds and 800ms inside 2s",
			contextLSPSeeds, contextLSPSubDeadline, contextExpansionDeadline)
	}
	var c ContextCollector
	if c.lspSubDeadline() != contextLSPSubDeadline {
		t.Errorf("the default sub-deadline is %s", c.lspSubDeadline())
	}
	c.lspDeadline = time.Second
	if c.lspSubDeadline() != time.Second {
		t.Errorf("an explicit sub-deadline is ignored: %s", c.lspSubDeadline())
	}
}
