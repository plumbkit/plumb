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
	"github.com/plumbkit/plumb/internal/paths"
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
      "description": "Zero-based line number of the symbol. Required when symbol_name is not provided."
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset within the line. Required when symbol_name is not provided."
    },
    "symbol_name": {
      "type": "string",
      "description": "Symbol name to look up instead of a position — PREFERRED over line/character. Accepts plain name or ReceiverType.MethodName form. plumb resolves it against the file's symbols, avoiding the off-by-one and 'no identifier found' errors of a hand-computed position. When provided, line and character are not needed."
    },
    "direction": {
      "type": "string",
      "enum": ["incoming", "outgoing", "both"],
      "description": "Which call direction to return: callers (incoming), callees (outgoing), or both. Defaults to both."
    }
  },
  "required": ["uri"],
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
		"PREFER a name (uri + symbol_name) — plumb resolves the exact identifier position for you, " +
		"avoiding off-by-one errors; a raw file position (uri + line + character) is the fallback " +
		"and, when it lands off an identifier, is snapped to the enclosing symbol. " +
		"Useful for understanding control flow and assessing the impact of changes. " +
		"When the language server provides no call hierarchy for the file (e.g. zls for Zig), " +
		"falls back to the topology call graph, annotated source=topology (approximate)."
}

type callHierarchyArgs struct {
	URI        string  `json:"uri"`
	Line       *uint32 `json:"line"`
	Character  *uint32 `json:"character"`
	SymbolName string  `json:"symbol_name"`
	Direction  string  `json:"direction,omitempty"`
}

// callHierarchyQuery is a request resolved to a concrete cursor position — from
// a raw line/character, a resolved symbol_name, or a snap — so the internal
// render/topology/snap helpers never re-derive a position.
type callHierarchyQuery struct {
	uri       string
	line      uint32
	character uint32
	direction string
	// symbolName is set only when plumb resolved line/character from a
	// symbol_name argument, so a server rejection is explained as a stale symbol
	// tree rather than as bad coordinates the caller never passed.
	symbolName string
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
	uri := toFileURIAnchored(a.URI, t.ws)

	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	if a.SymbolName != "" {
		return t.executeByName(ctx, uri, a.SymbolName, a.Direction)
	}
	if a.Line == nil || a.Character == nil {
		return "", fmt.Errorf("call_hierarchy: either symbol_name or both line and character are required")
	}
	q := callHierarchyQuery{uri: uri, line: *a.Line, character: *a.Character, direction: a.Direction}
	return t.executeByPosition(ctx, q, true)
}

// executeByName resolves the symbol by name against the file's document symbols
// and queries at its SelectionRange.Start — the off-by-one-proof path shared
// with find_references and get_definition. Multiple matches are rendered in
// turn.
func (t *CallHierarchy) executeByName(ctx context.Context, uri, name, direction string) (string, error) {
	syms, err := t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		return "", lspTimeoutErr("call_hierarchy", t.timeout, fmt.Errorf("resolving symbol %q: %w", name, err))
	}
	matches := resolveSymbolsByName(syms, name)
	if len(matches) == 0 {
		return fmt.Sprintf("No symbol named %q in %s.%s", name, uri, didYouMean(suggestSymbols(syms, name))), nil
	}
	if len(matches) == 1 {
		return t.executeByPosition(ctx, queryForSymbol(uri, matches[0], direction, name), false)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Call hierarchy for %q (%d symbol matches):\n", name, len(matches))
	for _, sym := range matches {
		fmt.Fprintf(&sb, "\n## %s (%s) line %d\n\n", sym.Name, symbolKindName(sym.Kind), sym.SelectionRange.Start.Line+1)
		result, err := t.executeByPosition(ctx, queryForSymbol(uri, sym, direction, name), false)
		if err != nil {
			fmt.Fprintf(&sb, "(error: %v)\n", err)
			continue
		}
		sb.WriteString(result)
	}
	return sb.String(), nil
}

// queryForSymbol builds a query at a resolved symbol's identifier position. name
// is the symbol_name the caller passed, recorded so a failure is reported as a
// name resolution rather than as a coordinate the caller chose.
func queryForSymbol(uri string, sym protocol.DocumentSymbol, direction, name string) callHierarchyQuery {
	return callHierarchyQuery{
		uri:        uri,
		line:       sym.SelectionRange.Start.Line,
		character:  sym.SelectionRange.Start.Character,
		direction:  direction,
		symbolName: name,
	}
}

func (t *CallHierarchy) executeByPosition(ctx context.Context, q callHierarchyQuery, allowSnap bool) (string, error) {
	items, err := t.client.PrepareCallHierarchy(ctx, protocol.PrepareCallHierarchyParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: q.uri},
		Position:     protocol.Position{Line: q.line, Character: q.character},
	})
	if err != nil || len(items) == 0 {
		// The server resolved no call-hierarchy item (it may not implement
		// prepareCallHierarchy at all). Try the topology call graph before
		// snapping or surfacing the original error / empty result.
		if out, ok := t.topologyCallHierarchy(ctx, q); ok {
			return out, nil
		}
		if err != nil {
			if allowSnap && isPositionMissErr(err) {
				return t.snapAndRetry(ctx, q)
			}
			return "", queryErr("call_hierarchy", q.symbolName, err)
		}
		return "No call hierarchy item found at the given position.", nil
	}
	return t.renderLSP(ctx, q, items[0])
}

// snapAndRetry recovers from a raw position that missed an identifier: it
// resolves the enclosing document symbol and re-queries once at its
// SelectionRange.Start (topology fallback already tried and unavailable). When
// nothing encloses the line it returns an actionable error naming nearby
// symbols. The retry passes allowSnap=false so a snap can never recurse.
func (t *CallHierarchy) snapAndRetry(ctx context.Context, q callHierarchyQuery) (string, error) {
	snapped, syms, ok := snapPosition(ctx, t.client, q.uri, q.line)
	if !ok {
		return "", positionMissErr("call_hierarchy", q.uri, q.line, syms)
	}
	snappedQ := q
	snappedQ.line = snapped.Line
	snappedQ.character = snapped.Character
	out, err := t.executeByPosition(ctx, snappedQ, false)
	if err != nil {
		return "", err
	}
	return snapNotice(q.uri, q.line, q.character, snapped.Line) + out, nil
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
func (t *CallHierarchy) renderLSP(ctx context.Context, q callHierarchyQuery, item protocol.CallHierarchyItem) (string, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Call hierarchy for %s (%s) at %s:%d\n\n",
		item.Name, symbolKindName(item.Kind), item.URI, item.Range.Start.Line+1)

	if q.direction == "incoming" || q.direction == "both" {
		incoming, err := t.client.IncomingCalls(ctx, protocol.CallHierarchyIncomingCallsParams{Item: item})
		if err != nil {
			return "", lspTimeoutErr("call_hierarchy", t.timeout, fmt.Errorf("incoming: %w", err))
		}
		writeCallHierarchySection(&sb, "Callers (incoming)", incomingTargets(incoming))
	}
	if q.direction == "outgoing" || q.direction == "both" {
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

// topologyCallHierarchy reconstructs a call hierarchy when the language server
// provides none (e.g. zls has no prepareCallHierarchy). Callers come from the
// language server's own find_references, mapped to the enclosing symbol at each
// call site — this catches callers the topology call graph misses (e.g. calls
// inside Zig `test "…" {}` blocks). Callees come from the topology call graph,
// since references cannot answer "what does this call". ok is false when neither
// source can resolve the symbol, so the caller keeps the original LSP behaviour.
func (t *CallHierarchy) topologyCallHierarchy(ctx context.Context, q callHierarchyQuery) (string, bool) {
	store := activeTopology(t.topo)
	if store == nil {
		return "", false
	}
	centre, ok := topologyCentre(ctx, store, q.uri, q.line)
	if !ok {
		return "", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Call hierarchy for %s (%s) at %s:%d "+
		"(reconstructed — this language server provides no call hierarchy; "+
		"callers via LSP references, callees via topology; approximate)\n\n",
		centre.Name, string(centre.Kind), centre.Path, centre.StartLine)
	if q.direction == "incoming" || q.direction == "both" {
		callers := t.lspCallers(ctx, q)
		if callers == nil {
			callers = topologyCallRefs(ctx, store, centre, topology.DirectionInward)
		}
		writeCallHierarchySection(&sb, "Callers (incoming)", callers)
	}
	if q.direction == "outgoing" || q.direction == "both" {
		writeCallHierarchySection(&sb, "Callees (outgoing)",
			topologyCallRefs(ctx, store, centre, topology.DirectionOutward))
	}
	return strings.TrimRight(sb.String(), "\n") + "\n", true
}

// lspCallers reconstructs the callers of the symbol at the query position from
// the language server's find_references: each reference (excluding the
// declaration) is mapped to the enclosing symbol in its file, so the result is
// named callers rather than bare call sites. Returns nil when the server has no
// references for the position (the caller then falls back to topology).
func (t *CallHierarchy) lspCallers(ctx context.Context, q callHierarchyQuery) []callRef {
	locs, err := t.client.References(ctx, protocol.ReferenceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: q.uri},
		Position:     protocol.Position{Line: q.line, Character: q.character},
		Context:      protocol.ReferenceContext{IncludeDeclaration: false},
	})
	if err != nil || len(locs) == 0 {
		return nil
	}
	symsByURI := make(map[string][]protocol.DocumentSymbol)
	refs := make([]callRef, 0, len(locs))
	for _, l := range locs {
		syms, ok := symsByURI[l.URI]
		if !ok {
			syms, _ = t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: l.URI},
			})
			symsByURI[l.URI] = syms
		}
		if enc := deepestEnclosingDocSymbol(syms, l.Range.Start.Line); enc != nil {
			refs = append(refs, callRef{
				name: enc.Name, kind: symbolKindName(enc.Kind),
				uri: l.URI, line: int(enc.SelectionRange.Start.Line) + 1,
			})
			continue
		}
		refs = append(refs, callRef{
			name: "(call site)", kind: "reference",
			uri: l.URI, line: int(l.Range.Start.Line) + 1,
		})
	}
	return refs
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
		topology.NodeHint{PathSubstr: filepath.Base(paths.URIToPath(uri))})
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
