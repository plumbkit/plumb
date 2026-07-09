package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// topologyStoreFn returns the active topology store, or nil when topology is
// disabled or the workspace is not yet attached. Mirrors the topoFn accessor
// the dedicated topology_* tools already use.
type topologyStoreFn = func() *topology.Store

// topologyFallbackNote prefixes every fallback response so the caller knows the
// answer came from the (possibly stale, heuristic) topology index rather than a
// live language server.
const topologyFallbackNote = "[topology fallback — LSP unavailable; results are approximate and may be stale. source=topology, mode=indexed-approximate]"

// LSPWarmupFn reports whether the language server that would serve uri — or the
// connection primary when uri is empty — is still warming, and for how long.
// Wired to the routing proxy's resolution-only WarmupStatus, so calling it never
// starts a server. Implementations must be safe for concurrent use — tools call
// it from concurrent Executes. Every call site is nil-safe: a nil fn means the
// warm-up state is unknown, so the genuinely-unavailable wording is used.
type LSPWarmupFn = func(uri string) (warming bool, elapsed time.Duration)

// lspWarmup resolves fn for uri, treating a nil fn as not warming.
func lspWarmup(fn LSPWarmupFn, uri string) (bool, time.Duration) {
	if fn == nil {
		return false, 0
	}
	return fn(uri)
}

// warmupElapsedSuffix renders the elapsed-time parenthetical for a warming note
// — " (~4s elapsed)" — rounded to whole seconds, or "" when the rounded elapsed
// time is zero (nothing useful to report).
func warmupElapsedSuffix(elapsed time.Duration) string {
	rounded := elapsed.Round(time.Second)
	if rounded <= 0 {
		return ""
	}
	return fmt.Sprintf(" (~%s elapsed)", rounded)
}

// topologyFallbackNoteFor picks the fallback banner for a symbol-query tool:
// the warming variant when the server that would own uri is still completing
// its handshake (so the agent retries instead of concluding the LSP is broken),
// else topologyFallbackNote — byte-identical to the historical text — for the
// genuinely-unavailable case.
func topologyFallbackNoteFor(fn LSPWarmupFn, uri string) string {
	warming, elapsed := lspWarmup(fn, uri)
	if !warming {
		return topologyFallbackNote
	}
	return fmt.Sprintf("[topology fallback — LSP still warming%s; results are approximate and may be stale; "+
		"semantic tools will answer once it is ready — retry shortly. source=topology, mode=indexed-approximate]",
		warmupElapsedSuffix(elapsed))
}

// activeTopology resolves the store from a nil-safe accessor.
func activeTopology(fn topologyStoreFn) *topology.Store {
	if fn == nil {
		return nil
	}
	return fn()
}

// topologyDisabledMessage is the single response every topology_* tool returns
// when the index is genuinely unavailable — topology disabled in config, or the
// workspace not yet attached (storeFn returns nil). It is deliberately distinct
// from a successful query that simply matched nothing: those return a
// tool-specific "no results"/"not found" message, so an agent is never told
// topology is off when it is actually indexed and working.
func topologyDisabledMessage() string {
	return "topology indexing is disabled for this session\n" +
		"Set [topology] enabled = true in .plumb/config.toml to enable."
}

// filterTopologyByName returns nodes whose name contains query (case-insensitive),
// mirroring the substring matching of find_symbol's LSP path.
func filterTopologyByName(nodes []topology.Node, query string) []topology.Node {
	q := strings.ToLower(query)
	out := make([]topology.Node, 0, len(nodes))
	for _, n := range nodes {
		if strings.Contains(strings.ToLower(n.Name), q) {
			out = append(out, n)
		}
	}
	return out
}

// formatTopologyMatches renders a name-lookup fallback result prefixed with
// note (topologyFallbackNote or its warming variant).
func formatTopologyMatches(note, header string, nodes []topology.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n%s:\n\n", note, header)
	if len(nodes) == 0 {
		sb.WriteString("(no matching symbols in the index)\n")
		return sb.String()
	}
	for _, n := range nodes {
		fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n", n.Name, string(n.Kind), n.Path, n.StartLine)
	}
	return sb.String()
}

// topologyFillNote prefixes a result that SUPPLEMENTS an available-but-empty LSP
// answer with index hits — distinct from topologyFallbackNote, which is for when
// the language server errored or timed out. The server is up here; a lazy server
// (zls and the other on-demand indexers) simply had not analysed the matching
// files yet, so the Map fills the gap rather than reporting a false "not found".
const topologyFillNote = "[topology fill — the language server returned no matches; supplementing from the index. source=topology, mode=indexed-approximate]"

// formatTopologyFill renders index hits that supplement an empty LSP result.
func formatTopologyFill(header string, nodes []topology.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n%s:\n\n", topologyFillNote, header)
	for _, n := range nodes {
		fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n", n.Name, string(n.Kind), n.Path, n.StartLine)
	}
	return sb.String()
}

// topologyDefinitionNote prefixes the get_definition fallback. It is deliberately
// explicit that the location is the symbol's DECLARATION line found by name, not
// the precise cursor target a language server would jump to: the index has no
// position-level go-to-definition, only declaration sites.
const topologyDefinitionNote = "[topology fallback — language server unavailable; located by symbol name, declaration line not cursor offset. source=topology, mode=indexed-approximate]"

// topologyDefinitionNoteFor picks the get_definition fallback banner: the
// warming variant when the server that would own uri is still completing its
// handshake, else topologyDefinitionNote — byte-identical to the historical
// text — for the genuinely-unavailable case.
func topologyDefinitionNoteFor(fn LSPWarmupFn, uri string) string {
	warming, elapsed := lspWarmup(fn, uri)
	if !warming {
		return topologyDefinitionNote
	}
	return fmt.Sprintf("[topology fallback — language server still warming%s; located by symbol name, "+
		"declaration line not cursor offset; semantic tools will answer once it is ready — retry shortly. "+
		"source=topology, mode=indexed-approximate]",
		warmupElapsedSuffix(elapsed))
}

// topologyDefinitionFallback resolves name to its declaration site(s) in the
// index and formats them prefixed with note (topologyDefinitionNote or its
// warming variant), or returns ("", false) when topology is unavailable or
// the name is unknown. get_definition uses it when the language server is
// unavailable (still warming, or erroring): approximate — the declaration line,
// not the exact definition the LSP would resolve — but it keeps navigation
// working while the server warms. A dotted name (ReceiverType.MethodName) retries
// on its final segment, mirroring the LSP name resolver.
func topologyDefinitionFallback(fn topologyStoreFn, note, name string) (string, bool) {
	store := activeTopology(fn)
	if store == nil {
		return "", false
	}
	ctx := context.Background()
	nodes, err := store.ResolveNodes(ctx, name, topology.NodeHint{})
	if err != nil || len(nodes) == 0 {
		if base := symbolBaseSegment(name); base != name {
			nodes, err = store.ResolveNodes(ctx, base, topology.NodeHint{})
		}
	}
	if err != nil || len(nodes) == 0 {
		return "", false
	}
	return formatTopologyDefinition(note, name, nodes), true
}

// formatTopologyDefinition renders a name-resolved definition fallback.
func formatTopologyDefinition(note, name string, nodes []topology.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nDeclaration of %q:\n\n", note, name)
	for _, n := range nodes {
		fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n", n.Name, string(n.Kind), n.Path, n.StartLine)
	}
	return sb.String()
}

// symbolBaseSegment returns the final dot-separated segment of name (the method
// name in ReceiverType.MethodName), or name itself when undotted.
func symbolBaseSegment(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}

// formatTopologyOutline renders a single-file outline fallback result prefixed
// with note (topologyFallbackNote or its warming variant).
func formatTopologyOutline(note, uri string, nodes []topology.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nSymbols in %s (%d, source: topology)\n\n", note, uri, len(nodes))
	for _, n := range nodes {
		if n.EndLine == 0 || n.StartLine == n.EndLine {
			fmt.Fprintf(&sb, "%s (%s) line %d\n", n.Name, string(n.Kind), n.StartLine)
		} else {
			fmt.Fprintf(&sb, "%s (%s) lines %d–%d\n", n.Name, string(n.Kind), n.StartLine, n.EndLine)
		}
	}
	return sb.String()
}
