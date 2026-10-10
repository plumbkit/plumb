package tools

import (
	"context"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_index.go — which topology index a call may consult.
//
// The index is keyed by the calling agent's canonical root (Invariant 3, B6). On a
// shared connection the connection holds one index, for the root IT is pinned to,
// while each logical agent may be pinned to a root of its own; answering an agent
// from the connection's index would describe someone else's tree with the
// authority of an index. So the collector never takes "the" index: it asks for the
// one open for the root it resolved for this agent, and treats nothing as a
// substitute for it.

// TopologyForRootFn returns the topology index that is open for a canonical
// workspace root, or nil when none is. It must answer from the root it is given
// and from nothing else: not from the connection's pin, and not by opening an
// index as a side effect of a read-only call.
type TopologyForRootFn func(root string) *topology.Store

// contextIndex is the topology index as this call may use it. store is nil when
// there is none for the agent's root, or when the accessor supplied one that
// describes a different root: mismatch then says so, so the pack can word the
// refusal without naming that other root, since naming it would disclose it.
type contextIndex struct {
	store    *topology.Store
	mismatch bool
}

// indexFor asks the root-keyed accessor for root's index and checks the answer.
// The check is defence in depth: a wiring mistake (an accessor that hands back the
// connection's store, say) must degrade to "no index" rather than answer an agent
// about a root that is not its own.
func (c *ContextCollector) indexFor(root string) contextIndex {
	if c.storeFor == nil {
		return contextIndex{}
	}
	store := c.storeFor(root)
	if store == nil {
		return contextIndex{}
	}
	if canonicalRoot(store.Root()) != root {
		return contextIndex{mismatch: true}
	}
	return contextIndex{store: store}
}

// unavailable words why symbols cannot be resolved, or "" when they can.
func (i contextIndex) unavailable() string {
	switch {
	case i.mismatch:
		return "the topology index supplied for this root describes another root and was not consulted, so symbols " +
			"cannot be resolved; seed by file"
	case i.store == nil:
		return "no topology index is available for this root (topology is disabled, or no session has opened it), " +
			"so symbols cannot be resolved; seed by file"
	}
	return ""
}

// usable reports whether the index may support relationship and test-impact
// claims: it exists, describes this root, and is not failing.
func (i contextIndex) usable() bool {
	return i.store != nil && !i.store.Health().Failing
}

// indexedHash is the content hash the index holds for rel at this moment, or "" when
// it holds none (the file is not indexed, was recorded without a parse, or the
// lookup failed). It is read straight after the node spans it vouches for, so that
// hash and span describe one version of the file: a reindex that lands later cannot
// make an older span look current, because the hash a span is judged by is the one
// captured with it, not the one the index holds when the body is sliced.
func indexedHash(ctx context.Context, store *topology.Store, rel string) string {
	if store == nil {
		return ""
	}
	hash, ok, err := store.IndexedContentHash(ctx, rel)
	if err != nil || !ok {
		return ""
	}
	return hash
}

// hashCache is indexedHash memoised for one walk, so a file costs one query however
// many of its declarations the walk reaches. The first answer for a file stays the
// answer: a node reached later was read no earlier than that, so a hash older than
// its span makes the span fail the comparison and be re-extracted, which is the safe
// direction. A lookup the deadline cut short is not remembered. It is for one
// goroutine.
func hashCache(ctx context.Context, store *topology.Store) func(rel string) string {
	seen := map[string]string{}
	return func(rel string) string {
		if h, ok := seen[rel]; ok {
			return h
		}
		h := indexedHash(ctx, store, rel)
		if ctx.Err() == nil {
			seen[rel] = h
		}
		return h
	}
}
