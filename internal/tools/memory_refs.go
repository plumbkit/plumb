package tools

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
)

// relatedMemoriesMax caps the related-memory lines appended to a topology
// tool response — the same slot discipline as the hint block.
const relatedMemoriesMax = 3

// relatedRefsMax caps how many topology nodes become CodeRefs for one match
// pass, so a huge neighbourhood cannot make the join pass expensive.
const relatedRefsMax = 30

// nodesToRefs converts topology nodes into the CodeRef join contract — stable
// fields only, never topology row IDs.
func nodesToRefs(nodes []topology.Node) []memory.CodeRef {
	if len(nodes) > relatedRefsMax {
		nodes = nodes[:relatedRefsMax]
	}
	refs := make([]memory.CodeRef, 0, len(nodes))
	for _, n := range nodes {
		refs = append(refs, memory.CodeRef{Kind: string(n.Kind), File: n.Path, SymbolName: n.Name})
	}
	return refs
}

// relatedMemoriesSection lists memories related to the given refs — names,
// descriptions, and the match reason only, never bodies. Returns "" when the
// workspace is unknown or nothing matches; the join never fails the tool.
func relatedMemoriesSection(ws string, refs []memory.CodeRef) string {
	if ws == "" || len(refs) == 0 {
		return ""
	}
	mems, err := memory.List(ws)
	if err != nil || len(mems) == 0 {
		return ""
	}
	hits := memory.MemoriesForRefs(mems, refs, relatedMemoriesMax)
	if len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nRelated memories (read_memory to view):\n")
	for _, h := range hits {
		fmt.Fprintf(&sb, "  - '%s'", h.Name)
		if h.Confidence != "" && h.Confidence != memory.ConfidenceUser {
			fmt.Fprintf(&sb, " [%s]", h.Confidence)
		}
		if h.Description != "" {
			fmt.Fprintf(&sb, " — %s", firstLine(h.Description))
		}
		fmt.Fprintf(&sb, " (%s)\n", h.Why)
	}
	return strings.TrimRight(sb.String(), "\n")
}
