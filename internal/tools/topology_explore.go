package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/topology"
)

var topologyExploreSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "Symbol or qualified name in the topology index."
    },
    "depth": {
      "type": "integer",
      "description": "BFS depth (default 2, max 4).",
      "default": 2
    },
    "max_nodes": {
      "type": "integer",
      "description": "Neighbours returned (default 50, max 200).",
      "default": 50
    },
    "max_bytes": {
      "type": "integer",
      "description": "Byte budget for neighbours (default 30000, max 100000).",
      "default": 30000
    },
    "include_source": {
      "type": "string",
      "description": "none (name and path), signatures (default), or docstrings (plus the first docstring line; 'snippets' and 'full' are aliases). For whole bodies use read_symbol.",
      "default": "signatures"
    },
    "edge_kinds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Edge kinds to follow: calls, imports, contains, defines, inherits, implements."
    },
    "path": {
      "type": "string",
      "description": "File-path substring to pick among same-named symbols."
    },
    "kind": {
      "type": "string",
      "description": "Node kind to pick among same-named symbols: function, method, type, class, …"
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`)

// TopologyExplore performs a bounded BFS neighbourhood around a named symbol.
//
// Concurrency: Execute is safe for concurrent use.
type TopologyExplore struct {
	storeFn func() *topology.Store
	ws      WorkspaceFn // optional; enables the related-memories join
}

// NewTopologyExplore returns a new TopologyExplore tool.
// storeFn returns the current topology.Store for the session, or nil if disabled.
func NewTopologyExplore(storeFn func() *topology.Store) *TopologyExplore {
	return &TopologyExplore{storeFn: storeFn}
}

// WithMemories wires the workspace accessor so the response can append
// memories related to the explored neighbourhood (CodeRef join).
func (t *TopologyExplore) WithMemories(ws WorkspaceFn) *TopologyExplore {
	t.ws = ws
	return t
}

func (*TopologyExplore) Name() string                 { return "topology_explore" }
func (*TopologyExplore) InputSchema() json.RawMessage { return topologyExploreSchema }
func (*TopologyExplore) Description() string {
	return "Bounded neighbourhood around a named symbol in the topology index: the centre, neighbours, edges and, for a type, its members. Narrow it first on a large file or unfamiliar language: include_source=\"none\" is several times smaller than the default signatures, and depth=1 with max_nodes=15 answers \"what touches this?\" cheaply. Approximate (source=topology): use the LSP tools for authoritative references and definitions, and read_symbol for whole bodies. Reports truncation."
}

type topologyExploreArgs struct {
	Name          string   `json:"name"`
	Depth         int      `json:"depth"`
	MaxNodes      int      `json:"max_nodes"`
	MaxBytes      int      `json:"max_bytes"`
	IncludeSource string   `json:"include_source"`
	EdgeKinds     []string `json:"edge_kinds"`
	Path          string   `json:"path"`
	Kind          string   `json:"kind"`
}

func (t *TopologyExplore) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTopologyExploreArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	store := t.storeFn()
	if store == nil {
		return topologyDisabledMessage(), nil
	}
	nb, alts, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", withIndexHealthErr(store, runErr)
	}
	out := formatTopologyNeighbourhood(nb, a, alts)
	if t.ws != nil {
		nodes := append([]topology.Node{nb.Centre}, nb.Nodes...)
		out += relatedMemoriesSection(t.ws(ctx), nodesToRefs(nodes))
	}
	return withIndexHealth(store, out), nil
}

func parseTopologyExploreArgs(raw json.RawMessage) (topologyExploreArgs, error) {
	var a topologyExploreArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_explore: invalid arguments: %w", err)
	}
	if a.IncludeSource == "" {
		a.IncludeSource = "signatures"
	}
	return a, nil
}

func (a *topologyExploreArgs) validate() error {
	if a.Name == "" {
		return errors.New("topology_explore: name is required")
	}
	return nil
}

func (t *TopologyExplore) run(ctx context.Context, store *topology.Store, a topologyExploreArgs) (*topology.Neighbourhood, []topology.Node, error) {
	if store == nil {
		return nil, nil, nil // defensive; Execute pre-checks a nil store
	}
	cands, err := store.ResolveNodes(ctx, a.Name, topology.NodeHint{PathSubstr: a.Path, Kind: a.Kind})
	if err != nil {
		return nil, nil, err
	}
	if len(cands) == 0 {
		return nil, nil, fmt.Errorf("topology: symbol %q not found in index", a.Name)
	}
	opts := topology.ExploreOpts{
		Depth:         a.Depth,
		MaxNodes:      topology.ClampToolNodes(a.MaxNodes),
		MaxBytes:      topology.ClampToolBytes(a.MaxBytes),
		IncludeSource: a.IncludeSource,
		EdgeKinds:     a.EdgeKinds,
	}
	nb, err := store.ExploreFrom(ctx, cands[0], opts)
	if err != nil {
		return nil, nil, err
	}
	return nb, cands[1:], nil
}

func formatTopologyNeighbourhood(nb *topology.Neighbourhood, a topologyExploreArgs, alts []topology.Node) string {
	if nb == nil {
		return fmt.Sprintf("topology_explore: symbol %q not found in the index", a.Name)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology explore: %s %q (source=topology)\n", string(nb.Centre.Kind), nb.Centre.Name)
	fmt.Fprintf(&sb, "  path: %s", nb.Centre.Path)
	if nb.Centre.StartLine > 0 {
		fmt.Fprintf(&sb, " L%d", nb.Centre.StartLine)
	}
	sb.WriteString("\n")
	if a.IncludeSource != "none" && nb.Centre.Signature != "" {
		fmt.Fprintf(&sb, "  sig:  %s\n", nb.Centre.Signature)
	}
	if wantsDocstring(a.IncludeSource) && nb.Centre.Docstring != "" {
		fmt.Fprintf(&sb, "  doc:  %s\n", firstLine(nb.Centre.Docstring))
	}
	sb.WriteString("\n")

	if len(nb.Nodes) == 0 {
		sb.WriteString("no neighbours found\n")
	} else {
		fmt.Fprintf(&sb, "neighbours (%d):\n", len(nb.Nodes))
		for _, n := range nb.Nodes {
			writeNeighbourLine(&sb, n, a.IncludeSource)
		}
	}

	if len(nb.Edges) > 0 {
		fmt.Fprintf(&sb, "\nedges (%d):\n", len(nb.Edges))
		for _, e := range nb.Edges {
			fmt.Fprintf(&sb, "  %d -[%s]-> %d (conf=%.2f)\n", e.FromID, string(e.Kind), e.ToID, e.Confidence)
		}
	}

	maxBytes := topology.ClampToolBytes(a.MaxBytes)
	writeMembersSection(&sb, nb, maxBytes)

	if nb.Truncated {
		sb.WriteString("\n[truncated: max_nodes or max_bytes reached — reduce depth or increase limits]\n")
	}
	banner := ""
	if nb.Truncated {
		banner = "the neighbourhood was cut at max_nodes or max_bytes. Nodes and edges " +
			"connected to this symbol are MISSING below — absence here is not evidence of " +
			"absence. Reduce depth, or raise max_nodes/max_bytes."
	}
	return withTruncationBanner(strings.TrimRight(sb.String(), "\n")+topologyAmbiguityNote(a.Name, alts), banner)
}

// topologyAmbiguityNote returns a trailing note when a symbol name resolved to
// more than one indexed node, listing the alternatives so the agent can re-query
// with a path/kind hint. Returns "" when the name was unambiguous.
func topologyAmbiguityNote(name string, alternatives []topology.Node) string {
	if len(alternatives) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n\n[note: %q matched %d symbols; showing the first. Pass path/kind to disambiguate. Other matches:",
		name, len(alternatives)+1)
	for _, n := range alternatives {
		fmt.Fprintf(&sb, "\n  %s %s — %s", string(n.Kind), n.Name, n.Path)
		if n.StartLine > 0 {
			fmt.Fprintf(&sb, " L%d", n.StartLine)
		}
	}
	sb.WriteString("]")
	return sb.String()
}

// writeMembersSection lists a type's members within maxBytes. Members a budget
// cuts — the traversal's (nb.MembersOmitted) or this response's — are never
// dropped silently: their count is written after the cut, outside the budget,
// like the truncation footer. The header counts every member found.
func writeMembersSection(sb *strings.Builder, nb *topology.Neighbourhood, maxBytes int) {
	total := len(nb.Members) + nb.MembersOmitted
	if total == 0 {
		return
	}
	count := strconv.Itoa(total)
	if nb.MembersCapped {
		count += "+"
	}
	header := fmt.Sprintf("\nmembers (%s):\n", count)
	if maxBytes > 0 && sb.Len()+len(header) > maxBytes {
		nb.Truncated = true
		fmt.Fprintf(sb, "\nmembers (%s): none listed, max_bytes reached — raise max_bytes to list them\n", count)
		return
	}
	sb.WriteString(header)
	shown := 0
	for _, m := range nb.Members {
		line := fmt.Sprintf("  %s %s — %s", string(m.Kind), m.Qualified, m.Path)
		if m.StartLine > 0 {
			line += fmt.Sprintf(" L%d", m.StartLine)
		}
		line += "\n"
		if maxBytes > 0 && sb.Len()+len(line) > maxBytes {
			nb.Truncated = true
			break
		}
		sb.WriteString(line)
		shown++
	}
	if omitted := total - shown; omitted > 0 {
		fmt.Fprintf(sb, "  … %d more member(s) omitted, max_bytes reached — raise max_bytes to list them\n", omitted)
	}
	if nb.MembersCapped {
		fmt.Fprintf(sb, "  … more members exist beyond the first %d — query workspace_symbols for the rest, "+
			"or explore one by its selector\n", topology.MemberListCap)
	}
}

func writeNeighbourLine(sb *strings.Builder, n topology.Node, includeSource string) {
	fmt.Fprintf(sb, "  %s %s — %s", string(n.Kind), n.Name, n.Path)
	if n.StartLine > 0 {
		fmt.Fprintf(sb, " L%d", n.StartLine)
	}
	sb.WriteString("\n")
	if includeSource != "none" && n.Signature != "" {
		fmt.Fprintf(sb, "    sig: %s\n", n.Signature)
	}
	if wantsDocstring(includeSource) && n.Docstring != "" {
		fmt.Fprintf(sb, "    doc: %s\n", firstLine(n.Docstring))
	}
}

// wantsDocstring reports whether the source mode includes docstrings (the
// richer "docstrings" mode, with "snippets"/"full" as aliases), as opposed to
// signatures alone.
func wantsDocstring(includeSource string) bool {
	return includeSource == "docstrings" || includeSource == "snippets" || includeSource == "full"
}

// firstLine returns the first non-empty line of s, trimmed and length-capped,
// so a multi-line docstring contributes one compact line to the output.
func firstLine(s string) string {
	for ln := range strings.SplitSeq(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			// Byte budget, rune-safe: this is an arbitrary source line, so a
			// plain ln[:120] lands mid-rune on any non-ASCII file.
			return textfmt.ClampBytes(ln, 120)
		}
	}
	return ""
}
