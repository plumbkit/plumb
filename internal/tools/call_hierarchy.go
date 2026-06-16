package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
)

var callHierarchySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path containing the symbol"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number of the symbol"
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset within the line"
    },
    "direction": {
      "type": "string",
      "enum": ["incoming", "outgoing", "both"],
      "description": "Which call direction to return: callers (incoming), callees (outgoing), or both. Defaults to both."
    }
  },
  "required": ["uri", "line", "character"],
  "additionalProperties": false
}`)

// CallHierarchy implements the call_hierarchy MCP tool.
type CallHierarchy struct {
	client  lsp.Client
	timeout time.Duration
	topo    topologyStoreFn // optional; topology call-graph fallback when the server has no call hierarchy
	ws      WorkspaceFn     // may be nil; anchors a workspace-relative uri to the pinned root
}

// NewCallHierarchy creates a CallHierarchy tool.
func NewCallHierarchy(client lsp.Client, timeout time.Duration) *CallHierarchy {
	return &CallHierarchy{client: client, timeout: timeout}
}

// WithTopologyFallback wires the topology store so the tool can answer from the
// indexed call graph when the language server provides no call hierarchy (e.g.
// zls does not implement textDocument/prepareCallHierarchy). Nil-safe.
func (t *CallHierarchy) WithTopologyFallback(fn topologyStoreFn) *CallHierarchy {
	t.topo = fn
	return t
}

// WithWorkspace anchors a relative uri to the pinned workspace root. Nil-safe.
func (t *CallHierarchy) WithWorkspace(ws WorkspaceFn) *CallHierarchy {
	t.ws = ws
	return t
}

func (t *CallHierarchy) Name() string                 { return "call_hierarchy" }
func (t *CallHierarchy) InputSchema() json.RawMessage { return callHierarchySchema }
func (t *CallHierarchy) Description() string {
	return "No native Claude Code equivalent. " +
		"Show the call hierarchy for a symbol: who calls it (incoming) and what it calls (outgoing). " +
		"Useful for understanding control flow and assessing the impact of changes. " +
		"When the language server provides no call hierarchy for the file (e.g. zls for Zig), " +
		"falls back to the topology call graph, annotated source=topology (approximate)."
}

type callHierarchyArgs struct {
	URI       string `json:"uri"`
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
	Direction string `json:"direction,omitempty"`
}

func parseCallHierarchyArgs(raw json.RawMessage) (callHierarchyArgs, error) {
	var a callHierarchyArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("call_hierarchy: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return a, fmt.Errorf("call_hierarchy: uri must not be empty")
	}
	if a.Direction == "" {
		a.Direction = "both"
	}
	return a, nil
}

func (t *CallHierarchy) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := parseCallHierarchyArgs(args)
	if err != nil {
		return "", err
	}
	a.URI = toFileURIAnchored(a.URI, t.ws)

	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	items, err := t.client.PrepareCallHierarchy(ctx, protocol.PrepareCallHierarchyParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: a.Line, Character: a.Character},
	})
	if err != nil || len(items) == 0 {
		// The server resolved no call-hierarchy item (it may not implement
		// prepareCallHierarchy at all). Try the topology call graph before
		// surfacing the original error / empty result.
		if out, ok := t.topologyCallHierarchy(ctx, a); ok {
			return out, nil
		}
		if err != nil {
			return "", positionErr("call_hierarchy", err)
		}
		return "No call hierarchy item found at the given position.", nil
	}
	return t.renderLSP(ctx, a, items[0])
}

// callRef is one rendered call-hierarchy entry (a caller or a callee), shared by
// the LSP and topology paths so both dedup and format identically. line is
// 1-based for display.
type callRef struct {
	name string
	kind string
	uri  string
	line int
}

// renderLSP formats the authoritative language-server call hierarchy.
func (t *CallHierarchy) renderLSP(ctx context.Context, a callHierarchyArgs, item protocol.CallHierarchyItem) (string, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Call hierarchy for %s (%s) at %s:%d\n\n",
		item.Name, symbolKindName(item.Kind), item.URI, item.Range.Start.Line+1)

	if a.Direction == "incoming" || a.Direction == "both" {
		incoming, err := t.client.IncomingCalls(ctx, protocol.CallHierarchyIncomingCallsParams{Item: item})
		if err != nil {
			return "", lspTimeoutErr("call_hierarchy", t.timeout, fmt.Errorf("incoming: %w", err))
		}
		writeCallHierarchySection(&sb, "Callers (incoming)", incomingTargets(incoming))
	}
	if a.Direction == "outgoing" || a.Direction == "both" {
		outgoing, err := t.client.OutgoingCalls(ctx, protocol.CallHierarchyOutgoingCallsParams{Item: item})
		if err != nil {
			return "", lspTimeoutErr("call_hierarchy", t.timeout, fmt.Errorf("outgoing: %w", err))
		}
		writeCallHierarchySection(&sb, "Callees (outgoing)", outgoingTargets(outgoing))
	}
	return strings.TrimRight(sb.String(), "\n") + "\n", nil
}

func incomingTargets(in []protocol.CallHierarchyIncomingCall) []callRef {
	refs := make([]callRef, 0, len(in))
	for _, c := range in {
		refs = append(refs, callRef{
			name: c.From.Name, kind: symbolKindName(c.From.Kind),
			uri: c.From.URI, line: int(c.From.Range.Start.Line) + 1,
		})
	}
	return refs
}

func outgoingTargets(out []protocol.CallHierarchyOutgoingCall) []callRef {
	refs := make([]callRef, 0, len(out))
	for _, c := range out {
		refs = append(refs, callRef{
			name: c.To.Name, kind: symbolKindName(c.To.Kind),
			uri: c.To.URI, line: int(c.To.Range.Start.Line) + 1,
		})
	}
	return refs
}

// writeCallHierarchySection renders one direction's entries under a heading,
// deduplicating by (name, uri, line). A language server may report the same
// callee once per call site (sourcekit-lsp does for a repeatedly-used property
// getter); the dedup collapses those to one line.
func writeCallHierarchySection(sb *strings.Builder, heading string, refs []callRef) {
	fmt.Fprintf(sb, "## %s\n\n", heading)
	if len(refs) == 0 {
		sb.WriteString("  (none)\n\n")
		return
	}
	seen := make(map[string]bool, len(refs))
	for _, r := range refs {
		key := fmt.Sprintf("%s\x00%s\x00%d", r.name, r.uri, r.line)
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Fprintf(sb, "- %s (%s) at %s:%d\n", r.name, r.kind, r.uri, r.line)
	}
	sb.WriteString("\n")
}

// topologyCallHierarchy answers a call-hierarchy query from the topology call
// graph when the language server provides none. It maps the position to the
// enclosing indexed symbol and follows "calls" edges: inward for callers,
// outward for callees. ok is false when topology is unavailable or the position
// resolves to no indexed symbol, so the caller keeps the original LSP behaviour.
func (t *CallHierarchy) topologyCallHierarchy(ctx context.Context, a callHierarchyArgs) (string, bool) {
	store := activeTopology(t.topo)
	if store == nil {
		return "", false
	}
	centre, ok := topologyCentre(ctx, store, a.URI, a.Line)
	if !ok {
		return "", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Call hierarchy for %s (%s) at %s:%d "+
		"(source=topology — approximate; this language server provides no call hierarchy)\n\n",
		centre.Name, string(centre.Kind), centre.Path, centre.StartLine)
	if a.Direction == "incoming" || a.Direction == "both" {
		writeCallHierarchySection(&sb, "Callers (incoming)",
			topologyCallRefs(ctx, store, centre, topology.DirectionInward))
	}
	if a.Direction == "outgoing" || a.Direction == "both" {
		writeCallHierarchySection(&sb, "Callees (outgoing)",
			topologyCallRefs(ctx, store, centre, topology.DirectionOutward))
	}
	return strings.TrimRight(sb.String(), "\n") + "\n", true
}

// topologyCentre maps a file position to the smallest enclosing indexed symbol,
// re-parsing the current file to find the name then resolving it (with edges) in
// the persisted graph.
func topologyCentre(ctx context.Context, store *topology.Store, uri string, line uint32) (topology.Node, bool) {
	nodes, err := store.ExtractFile(ctx, uri)
	if err != nil || len(nodes) == 0 {
		return topology.Node{}, false
	}
	enc := enclosingNode(nodes, line)
	if enc.Name == "" {
		return topology.Node{}, false
	}
	cands, err := store.ResolveNodes(ctx, enc.Name,
		topology.NodeHint{PathSubstr: filepath.Base(strings.TrimPrefix(uri, "file://"))})
	if err != nil || len(cands) == 0 {
		return topology.Node{}, false
	}
	centre := cands[0]
	for _, c := range cands {
		if c.StartLine == enc.StartLine {
			centre = c
			break
		}
	}
	return centre, true
}

// enclosingNode returns the smallest node whose 1-based line range contains the
// 0-based line, or a zero Node when none do.
func enclosingNode(nodes []topology.Node, line uint32) topology.Node {
	target := int(line) + 1
	var best topology.Node
	bestSpan := math.MaxInt
	for _, n := range nodes {
		if n.StartLine <= target && target <= n.EndLine {
			if span := n.EndLine - n.StartLine; span < bestSpan {
				bestSpan = span
				best = n
			}
		}
	}
	return best
}

// topologyCallRefs follows "calls" edges one hop from centre in dir (inward =
// callers, outward = callees) and returns the neighbour nodes as call entries.
func topologyCallRefs(ctx context.Context, store *topology.Store, centre topology.Node, dir topology.Direction) []callRef {
	nb, err := store.ExploreFrom(ctx, centre, topology.ExploreOpts{
		Depth:         1,
		MaxNodes:      200,
		MaxBytes:      50000,
		IncludeSource: "none",
		EdgeKinds:     []string{string(topology.EdgeCalls)},
		Direction:     dir,
	})
	if err != nil || nb == nil {
		return nil
	}
	refs := make([]callRef, 0, len(nb.Nodes))
	for _, n := range nb.Nodes {
		refs = append(refs, callRef{name: n.Name, kind: string(n.Kind), uri: n.Path, line: n.StartLine})
	}
	return refs
}
