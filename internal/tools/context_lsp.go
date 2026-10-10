package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_lsp.go — the language server's word on the top symbol seeds.
//
// The structural pack is complete without it: relationships come from the topology
// index, and the pack says so. When a language server is wired and ready, the first
// few callable symbol seeds are also put to it (call hierarchy: who calls the seed,
// whom it calls). What the server confirms of an edge the index already drew is
// raised to evidence 4; what only the server finds (typically a receiver-method
// caller, which the syntactic graph cannot resolve) is added at evidence 4 too.
//
// Everything about it is bounded and optional. The whole refinement shares one
// sub-deadline inside the call's own, each round trip is abandoned when that passes
// (a server that ignores its context cannot hold the pack), and a server that is
// absent, still warming, slow or in error leaves the structural pack as it was, with
// the reason said in its own line. LSP state is a freshness component of its own,
// never folded into the index's or the snapshot's (Invariant 8).

// Limits of the refinement (gate v1: lsp_enrichment_seeds, lsp_subdeadline_ms).
const (
	contextLSPSeeds       = 3
	contextLSPSubDeadline = 800 * time.Millisecond
	// contextLSPEdges bounds the edges taken from one answer, so a hub with a
	// thousand callers costs the pack a bounded mapping, and says it did.
	contextLSPEdges = 24
)

const (
	sourceLSP = "lsp"
	// labelLSPUnavailable leads every line that says the server did not (fully)
	// answer, so a reader sees one phrase whatever the reason.
	labelLSPUnavailable = "structural-only partial result; LSP enrichment unavailable"
)

// ContextLSP is the slice of the language-server client the refinement uses.
// lsp.Client satisfies it; a test supplies its own.
type ContextLSP interface {
	DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error)
	PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error)
	IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error)
	OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error)
}

// WithLSP wires the language server and its warm-up probe. Without it the pack is
// structural and says so.
func (c *ContextCollector) WithLSP(client ContextLSP, warmup LSPWarmupFn) *ContextCollector {
	c.lsp, c.lspWarm = client, warmup
	return c
}

type lspState int

const (
	lspOff     lspState = iota // nothing to ask: no callable symbol seed, or no usable index
	lspRefined                 // the server answered
	lspCut                     // the sub-deadline (or the call's own) passed first
	lspAbsent                  // no server, or it failed
	lspWarming                 // the server is still starting: it was not asked
)

// contextLSP is what the refinement did, for the pack's disclosures.
type contextLSP struct {
	State     lspState
	Asked     int           // top callable seeds put to the server
	Answered  int           // of those, the ones it answered for
	Confirmed int           // structural call edges the answers confirmed (raised to e4)
	Added     int           // call edges only the server found (e4)
	Unplaced  int           // server edges no indexed declaration could be matched to
	More      int           // server edges beyond contextLSPEdges per answer
	Why       string        // the cause, for a state that is not refined; a caveat for one that is
	Budget    time.Duration // the sub-deadline the refinement ran under
}

// gap is the pack's line for LSP state. A refinement that did not run says what the
// pack is made of instead.
func (l contextLSP) gap() string {
	switch l.State {
	case lspRefined:
		s := fmt.Sprintf("lsp: language server consulted for %d of %d top callable symbol seed(s): %d structural call edge(s) confirmed, %d added (e4); everything else in the pack is structural",
			l.Answered, l.Asked, l.Confirmed, l.Added)
		return s + l.caveats()
	case lspCut:
		return fmt.Sprintf("lsp: %s: cut at the %s sub-deadline after %d of %d seed(s) answered (%d edge(s) confirmed and %d added before the cut are kept); the structural pack is otherwise unchanged%s",
			labelLSPUnavailable, l.Budget, l.Answered, l.Asked, l.Confirmed, l.Added, l.caveats())
	case lspAbsent, lspWarming:
		return fmt.Sprintf("lsp: %s: %s", labelLSPUnavailable, l.Why)
	}
	return labelStructural
}

// caveats words what a refinement that ran could not place or take.
func (l contextLSP) caveats() string {
	var parts []string
	if l.Unplaced > 0 {
		parts = append(parts, fmt.Sprintf("%d server edge(s) could not be placed in the index (it may be out of date) and were left out", l.Unplaced))
	}
	if l.More > 0 {
		parts = append(parts, fmt.Sprintf("%d further server edge(s) beyond %d per answer were not taken", l.More, contextLSPEdges))
	}
	if l.Why != "" && l.State != lspAbsent && l.State != lspWarming {
		parts = append(parts, l.Why)
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

// lspSubDeadline is the budget the refinement runs under.
func (c *ContextCollector) lspSubDeadline() time.Duration {
	if c.lspDeadline > 0 {
		return c.lspDeadline
	}
	return contextLSPSubDeadline
}

// refineLSP puts the first few callable symbol seeds to the language server and
// folds what it confirms into the walk's pool, before the pool is ranked. It never
// fails: whatever goes wrong is the result's state.
func (c *ContextCollector) refineLSP(ctx context.Context, ex *expander, root string) contextLSP {
	seeds := callableSeeds(ex.seeds, contextLSPSeeds)
	switch {
	case len(seeds) == 0:
		return contextLSP{}
	case c.lsp == nil:
		return contextLSP{State: lspAbsent, Why: "no language server is wired for this call"}
	}
	uri := toFileURI(absUnder(root, seeds[0].node.Path))
	if warming, elapsed := lspWarmup(c.lspWarm, uri); warming {
		return contextLSP{State: lspWarming, Why: "the language server is still warming" + warmupElapsedSuffix(elapsed) + ", so it was not asked"}
	}
	budget := c.lspSubDeadline()
	lctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	run := &lspRun{client: c.lsp, ex: ex, root: root, graph: ctx}
	for _, s := range seeds {
		if lctx.Err() != nil {
			break
		}
		run.seed(lctx, s)
	}
	return run.result(lctx, budget)
}

// callableSeeds are the first n symbol seeds a call hierarchy can be asked about, in
// the order the caller named them.
func callableSeeds(seeds []hopNode, n int) []hopNode {
	var out []hopNode
	for _, s := range seeds {
		if callableKind(s.node.Kind) && len(out) < n {
			out = append(out, s)
		}
	}
	return out
}

// lspRun is one refinement's working state. It is for one goroutine.
type lspRun struct {
	client ContextLSP
	ex     *expander
	root   string
	// graph is the call's graph context. The index lookups that place a server edge
	// run under it, not under the sub-deadline: a refinement that ran out of time
	// must not make the walk look as if the index had.
	graph    context.Context
	res      contextLSP
	err      error // the first failure that was not a deadline
	timedOut bool  // a round trip failed on a deadline
}

// lspCall runs one round trip and gives up on it when ctx ends. A client is expected
// to honour its context; this is the guarantee for one that does not, because a pack
// must never wait on a server. The abandoned call finishes on its own: its result
// goes to a channel nobody reads, which is buffered so that it can.
func lspCall[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	type reply struct {
		v   T
		err error
	}
	done := make(chan reply, 1)
	go func() {
		v, err := fn(ctx)
		done <- reply{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// fail records a failed round trip: a deadline is a timeout, anything else is a
// cause worth naming (the first one).
func (r *lspRun) fail(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.DeadlineExceeded):
		r.timedOut = true
	case r.err == nil && !errors.Is(err, context.Canceled):
		r.err = err
	}
	return true
}

// seed asks the server about one seed and merges what it says. A seed counts as
// answered when the server resolved it and answered in at least one direction, so a
// server with callers but no callees (or the reverse) still contributes.
func (r *lspRun) seed(ctx context.Context, s hopNode) {
	r.res.Asked++
	item, ok := r.item(ctx, s)
	if !ok {
		return
	}
	incoming, err := lspCall(ctx, func(c context.Context) ([]protocol.CallHierarchyIncomingCall, error) {
		return r.client.IncomingCalls(c, protocol.CallHierarchyIncomingCallsParams{Item: item})
	})
	answered := !r.fail(err)
	if answered {
		callers := make([]protocol.CallHierarchyItem, 0, len(incoming))
		for _, call := range incoming {
			callers = append(callers, call.From)
		}
		r.mergeAll(s, callers, true)
	}
	outgoing, err := lspCall(ctx, func(c context.Context) ([]protocol.CallHierarchyOutgoingCall, error) {
		return r.client.OutgoingCalls(c, protocol.CallHierarchyOutgoingCallsParams{Item: item})
	})
	if !r.fail(err) {
		answered = true
		callees := make([]protocol.CallHierarchyItem, 0, len(outgoing))
		for _, call := range outgoing {
			callees = append(callees, call.To)
		}
		r.mergeAll(s, callees, false)
	}
	if answered {
		r.res.Answered++
	}
}

// mergeAll folds the first contextLSPEdges items of one answer into the pool and
// counts the rest.
func (r *lspRun) mergeAll(seed hopNode, items []protocol.CallHierarchyItem, inward bool) {
	if len(items) > contextLSPEdges {
		r.res.More += len(items) - contextLSPEdges
		items = items[:contextLSPEdges]
	}
	for _, it := range items {
		r.merge(seed, it, inward)
	}
}

// item resolves the seed to the server's call-hierarchy item: the declaration's
// identifier position comes from the server's own document symbols, chosen by the
// seed's name and span exactly as find_references does.
func (r *lspRun) item(ctx context.Context, s hopNode) (protocol.CallHierarchyItem, bool) {
	uri := toFileURI(absUnder(r.root, s.node.Path))
	syms, err := lspCall(ctx, func(c context.Context) ([]protocol.DocumentSymbol, error) {
		return r.client.DocumentSymbols(c, protocol.DocumentSymbolParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri}})
	})
	if r.fail(err) {
		return protocol.CallHierarchyItem{}, false
	}
	target, ok := crossFileTarget(syms, s.node.Name, s.node.StartLine, s.node.EndLine)
	if !ok {
		r.note("the server's symbols for " + textfmt.TerminalSafeLine(s.node.Path) + " do not match " + textfmt.TerminalSafeLine(s.sel) + " (it may have changed since indexing)")
		return protocol.CallHierarchyItem{}, false
	}
	items, err := lspCall(ctx, func(c context.Context) ([]protocol.CallHierarchyItem, error) {
		return r.client.PrepareCallHierarchy(c, protocol.PrepareCallHierarchyParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri}, Position: target.SelectionRange.Start,
		})
	})
	if r.fail(err) {
		return protocol.CallHierarchyItem{}, false
	}
	if len(items) == 0 {
		r.note("the server resolved no call-hierarchy item for " + textfmt.TerminalSafeLine(s.sel))
		return protocol.CallHierarchyItem{}, false
	}
	return items[0], true
}

// note keeps the first caveat about a seed the server could not answer for.
func (r *lspRun) note(why string) {
	if r.res.Why == "" {
		r.res.Why = why
	}
}

// merge folds one server edge into the pool: it confirms the structural edge the
// index drew between the same two declarations, or adds a call the index could not
// resolve. inward means the item calls the seed. The item is matched to an indexed
// declaration by position and name, and an edge that matches none is counted, not
// guessed at.
func (r *lspRun) merge(seed hopNode, it protocol.CallHierarchyItem, inward bool) {
	n, ok := r.declaration(it)
	if !ok {
		return
	}
	if n.ID == seed.node.ID || r.ex.isSeed(n.ID) {
		return
	}
	if i := r.ex.keptIndex(n.ID); i >= 0 {
		if r.ex.kept[i].confirmsCall(seed.node.ID, inward) {
			r.ex.kept[i].Evidence, r.ex.kept[i].Source, r.ex.kept[i].Conf = evidenceLSP, sourceLSP, 1.0
			r.ex.rk.score(&r.ex.kept[i])
			r.res.Confirmed++
		}
		return
	}
	rel := r.ex.makeRelated(n, seed, evidenceLSP, sourceLSP, 1.0, viaText(topology.EdgeCalls, inward, seed.sel))
	rel.Edge = topology.EdgeCalls
	if inward {
		rel.CallerOf = seed.node.ID
	}
	r.ex.keep(rel)
	r.res.Added++
}

// isSeed reports whether id is one of the symbol seeds the walk started from.
func (e *expander) isSeed(id int64) bool {
	for _, s := range e.seeds {
		if s.node.ID == id {
			return true
		}
	}
	return false
}

// keptIndex is the position of node id in the pool, or -1.
func (e *expander) keptIndex(id int64) int {
	for i := range e.kept {
		if e.kept[i].Node.ID == id {
			return i
		}
	}
	return -1
}

// confirmsCall reports whether r is the relationship the server just confirmed: a
// call between it and the seed, in the direction the server named. A node related
// some other way (a member, a same-file mate) is not confirmed by a call.
func (r contextRelated) confirmsCall(seedID int64, inward bool) bool {
	if r.Edge != topology.EdgeCalls || r.Evidence == evidenceLSP {
		return false
	}
	if inward {
		return r.CallerOf == seedID
	}
	return r.CallerOf == 0 && r.ParentID == seedID
}

// declaration places a server item in the index: the smallest indexed declaration of
// its file that encloses the item's identifier, accepted only if its name is the
// item's. A path outside the root, a declaration the scope does not admit, and an
// item the index cannot match are all left out; the last is counted.
func (r *lspRun) declaration(it protocol.CallHierarchyItem) (topology.Node, bool) {
	rel := relWithinRoot(r.root, paths.URIToPath(it.URI))
	if rel == "" {
		return topology.Node{}, false
	}
	n := enclosingNode(r.ex.declsOf(r.graph, rel), it.SelectionRange.Start.Line)
	if n.Name == "" || !itemNamesNode(it.Name, n) {
		r.res.Unplaced++
		return topology.Node{}, false
	}
	return n, r.ex.allowed(n)
}

// itemNamesNode reports whether a server's name for a declaration is the index's: a
// language server may spell a method with its receiver ("(*Cart).Total", "Cart.Total")
// where the index holds the bare name, or the reverse.
func itemNamesNode(name string, n topology.Node) bool {
	return name == n.Name || name == nodeSelector(n) || strings.HasSuffix(name, "."+n.Name)
}

// result is the refinement's outcome. A deadline that passed is a timeout whatever
// else happened; a failure with nothing answered is unavailability; otherwise the
// server was consulted.
func (r *lspRun) result(ctx context.Context, budget time.Duration) contextLSP {
	res := r.res
	res.Budget = budget
	switch {
	case ctx.Err() != nil || r.timedOut:
		res.State = lspCut
	case r.err != nil && res.Answered == 0:
		res.State, res.Why = lspAbsent, "the language server did not answer: "+textfmt.ClampBytes(textfmt.TerminalSafeLine(r.err.Error()), 120)
	case res.Answered == 0:
		res.State = lspAbsent
		if res.Why == "" {
			res.Why = "the language server had nothing for the seeds"
		}
	default:
		res.State = lspRefined
		if r.err != nil {
			res.Why = "one request failed: " + textfmt.ClampBytes(textfmt.TerminalSafeLine(r.err.Error()), 120)
		}
	}
	return res
}
