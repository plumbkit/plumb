package tools

import (
	"context"
	"slices"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_structure.go — what expansion adds beyond call edges: a type's members
// and the declarations that share a reached file.

// members lists a type's members (C05). They are structure the index records, not
// call edges: nothing here is attributed to the type as a caller or callee. A Go
// struct's fields are not graph members, which the pack says rather than letting a
// short member list read as the whole type.
func (e *expander) members(ctx context.Context, h hopNode) []contextRelated {
	if h.node.Kind != topology.KindType && h.node.Kind != topology.KindClass {
		return nil
	}
	ms, err := e.store.TypeMembers(ctx, h.node, topology.MemberListCap+1)
	if err != nil {
		e.fail(ctx, err)
		return nil
	}
	if len(ms) > topology.MemberListCap {
		ms = ms[:topology.MemberListCap]
		e.stats.MembersCapped = true
	}
	e.stats.MembersListed = e.stats.MembersListed || h.node.Language == "go"
	var out []contextRelated
	for _, m := range ms {
		if m.ID == h.node.ID || !e.allowed(m) {
			continue
		}
		out = append(out, e.makeRelated(m, h, evidenceExtractor, sourceExtractor, 1.0, "member of "+h.sel))
	}
	return out
}

// mates adds the declarations that share a file the walk reached. The Go
// extractor emits no containment edge for a package-level variable, so a table
// like pricing's `codes`, which a reached function reads, is otherwise invisible.
// It is added once per file, only for files that are not seeds', and only where
// the walk still has a hop to spend. Sharing a file is a hint of relevance, not a
// relationship the graph recorded, so it is ranked as heuristic evidence: at the
// same distance and prose coverage it sits below every edge.
func (e *expander) mates(ctx context.Context, h hopNode) []contextRelated {
	p := h.node.Path
	if h.dist < 1 || h.dist >= contextExpansionDepth || e.seedFiles[p] || e.mated[p] {
		return nil
	}
	e.mated[p] = true
	var picks []contextRelated
	for _, n := range e.declsOf(ctx, p) {
		if n.ID == h.node.ID || n.Kind == topology.KindTest || !e.allowed(n) {
			continue
		}
		picks = append(picks, e.makeRelated(n, h, evidenceHeuristic, "", 0, "same file as "+h.sel))
	}
	slices.SortFunc(picks, compareRelated)
	if len(picks) > contextFileMates {
		e.stats.Truncated += len(picks) - contextFileMates
		picks = picks[:contextFileMates]
	}
	return picks
}
