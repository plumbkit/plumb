package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Workspace-symbol fan-out: answering a URI-LESS query across every language
// server a workspace drives, rather than the connection's primary alone. Split
// out of routing_proxy.go, which routes per-file requests; this file is the one
// query that has no file to route by and therefore has to ask everybody.

// lsTarget is a (root, language) pair to query during workspace-symbol fan-out.
type lsTarget struct {
	root     string
	language string
}

// fanOutWorkspaceSymbols queries every language server in a monorepo root — the
// discovered child roots plus any already-attached entry under the workspace —
// and merges the results, deduplicating by symbol identity. Lazily-attached
// children are warmed on the first such query (then cached), so a no-file symbol
// search spans every detected language. A server that errors or is not yet ready
// is skipped; the merged result wins over a single failure, and only an
// all-error fan-out surfaces an error.
func (r *routingProxy) fanOutWorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams, wsRoot string, discovered []discoveredRoot) ([]protocol.SymbolInformation, error) {
	var (
		merged   []protocol.SymbolInformation
		seen     = map[string]bool{}
		firstErr error
		gotAny   bool
	)
	// targets CAN be empty, and an earlier version of this claimed otherwise.
	// WorkspaceSymbols only reaches the fan-out with a non-empty discovered set —
	// but agentWorkspaceSymbols calls it with discovered = nil for a declared
	// agent's own root, so when nothing is attached under that root there is
	// nothing to query. Handled at the warming return below.
	targets := r.symbolTargets(wsRoot, discovered)
	// ONE budget for WAITING, shared across targets, and it bounds only the wait.
	// A workspace can have several cold servers, and warmCap each would block
	// 4×warmCap on a cold daemon; one shared bound keeps the answer late by at
	// most what a single-language root already accepts.
	//
	// It is deliberately NOT the context the queries run on, and the first version
	// of this got that wrong: bounding the whole fan-out meant a cold target
	// ordered first consumed the budget, and every warm target after it then
	// issued its query on an ALREADY-EXPIRED context and failed with "context
	// deadline exceeded". Their symbols were lost — results this fan-out returned
	// instantly before the change — and the call surfaced that error instead. Since
	// symbolTargets lists discovered child roots before the attached entries, one
	// cold lazily-discovered child starved the primary, which is the commonest
	// monorepo shape. A warm target must cost nothing but its own query.
	waitCtx, cancel := context.WithTimeout(ctx, r.warmCap)
	defer cancel()
	for _, t := range targets {
		syms, ready, err := r.symbolsFrom(ctx, waitCtx, t, params)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !ready {
			continue
		}
		gotAny = true
		for _, sym := range syms {
			k := symbolKey(sym)
			if seen[k] {
				continue
			}
			seen[k] = true
			merged = append(merged, sym)
		}
	}
	if !gotAny {
		if firstErr != nil {
			return nil, firstErr
		}
		// No target was ready and none failed: every server is still warming.
		// Reporting that as an empty SUCCESS is the defect this branch exists to
		// close — "no symbols in this workspace" and "no server could answer yet"
		// are opposite facts, and the caller cannot tell them apart. The primary
		// path has always said so through warmingErr; the fan-out said nothing and
		// returned an empty slice.
		//
		// Only when there WERE targets. With none, nothing is warming and the honest
		// answer is an empty one: agentWorkspaceSymbols fans out over a declared
		// agent's own root with discovered = nil, so an agent whose root has no
		// attached server yet would otherwise be told its workspace is "still
		// warming" when there is no server to wait for.
		//
		// This guard was removed once, on the reasoning that the fan-out is only
		// entered with a non-empty discovered set — true of WorkspaceSymbols, and
		// mutation agreed, because nothing exercised the other caller: that caller
		// did not exist yet. It arrived from main while this branch was open. A guard
		// is not dead because one caller cannot trip it.
		if len(targets) > 0 {
			return nil, warmingErr(r.longestWarmup(targets), wsRoot)
		}
	}
	return merged, nil
}

// longestWarmup reports how long the slowest of these targets has been warming,
// for the warming error's "(N elapsed)" hint. Zero when the pool knows of no
// warmup, which warmingErr renders as the plain not-yet-ready message.
func (r *routingProxy) longestWarmup(targets []lsTarget) time.Duration {
	var longest time.Duration
	for _, t := range targets {
		if _, elapsed := r.pool.warmupFor(t.root, t.language); elapsed > longest {
			longest = elapsed
		}
	}
	return longest
}

// symbolTargets is the deduplicated (root, language) set to query for a
// workspace-wide symbol search: the discovered child roots first, then any other
// language server already attached under the workspace root (a lazily-routed
// secondary, or the elected primary itself).
func (r *routingProxy) symbolTargets(wsRoot string, discovered []discoveredRoot) []lsTarget {
	seen := map[lsTarget]bool{}
	var targets []lsTarget
	add := func(root, language string) {
		t := lsTarget{root: root, language: language}
		if !seen[t] {
			seen[t] = true
			targets = append(targets, t)
		}
	}
	for _, d := range discovered {
		add(d.root, d.language)
	}
	for _, e := range r.pool.entriesUnderRoot(wsRoot) {
		add(e.root, e.language)
	}
	return targets
}

// symbolsFrom acquires (without pinning) the server for one target and queries
// it. ready is false when the server is not yet warm (treat as no results, not
// an error); err is the query/acquire failure.
//
// It WAITS for a warming server, through the same entryClient(wait=true) policy
// the primary path uses, rather than taking a single non-blocking handle check.
// A URI-less query is the one call that cannot be retried usefully by the
// caller — an agent asking "where is Foo defined?" gets an empty answer and
// concludes the symbol does not exist — so the fan-out must not report a cold
// server as an empty workspace.
//
// TWO contexts, and the split is the whole point. waitCtx carries the fan-out's
// shared warm-up budget, so N cold targets cannot stack N×warmCap; ctx is the
// caller's own, and the QUERY runs on it. Running the query on waitCtx instead
// — the first version of this — meant a target reached after an earlier one had
// spent the budget issued its query on an expired context and failed, losing the
// symbols of a server that was warm and ready all along. A warm target costs its
// own query and nothing else, whatever preceded it.
func (r *routingProxy) symbolsFrom(ctx, waitCtx context.Context, t lsTarget, params protocol.WorkspaceSymbolParams) (syms []protocol.SymbolInformation, ready bool, err error) {
	e, err := r.pool.acquireLang(ctx, t.root, t.language, false)
	if err != nil {
		return nil, false, err
	}
	c := r.entryClient(waitCtx, e, true)
	if c == nil {
		return nil, false, nil
	}
	syms, err = c.WorkspaceSymbols(ctx, params)
	if err != nil {
		return nil, false, err
	}
	return syms, true, nil
}

// symbolKey identifies a symbol for fan-out deduplication: name, kind, and
// source location. Distinct servers cover disjoint subtrees so collisions are
// rare, but a file on a root boundary could surface twice.
func symbolKey(s protocol.SymbolInformation) string {
	loc := s.Location
	return fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%d", s.Name, s.Kind, loc.URI, loc.Range.Start.Line, loc.Range.Start.Character)
}
