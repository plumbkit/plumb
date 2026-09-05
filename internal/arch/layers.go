// Package arch declares plumb's layered architecture as data, so the rule
// "lower layers must never import higher ones" can be enforced by a test
// instead of by everyone remembering it.
//
// The layering was documented in AGENTS.md long before it was checked. It
// happened to hold — but nothing stopped it drifting, and an import that
// inverts a layer is invisible in review: it compiles, it passes, and it is
// only felt later as a dependency cycle that forces an awkward interface or a
// package split. With several agents editing the tree at once, "everyone
// remembers the rule" is not a mechanism.
//
// Concurrency: all state here is read-only package-level data, safe for
// concurrent use.
package arch

// Layer is a rung in the dependency ladder. Lower values are more fundamental:
// a package may import its own layer or any layer below it, never above.
type Layer int

// The layers, lowest first. The ordering IS the rule — comparison of these
// values is what the test enforces.
const (
	// LayerFoundation is stdlib-adjacent shared primitives: path resolution,
	// durable-write helpers, tokenisation, redaction, colour data, pure
	// rendering. Nothing here may import any other plumb package outside this
	// layer.
	LayerFoundation Layer = iota

	// LayerTransport is the wire: MCP server/registry and the LSP client,
	// JSON-RPC, protocol types, file watcher, and per-language adapters. It sits
	// low because it defines protocol vocabulary that upper layers speak; it
	// knows nothing about tools, topology, or the UI.
	LayerTransport

	// LayerDomain is plumb's own concepts: config, sessions, stats, memory,
	// collab, capability registries, the symbol cache.
	LayerDomain

	// LayerIntelligence is the topology index and its language extractors — the
	// structural understanding built on top of domain concepts.
	LayerIntelligence

	// LayerApplication is the MCP tool implementations: composite behaviour
	// assembled from every layer below.
	LayerApplication

	// LayerPresentation is the surfaces a human or client sees: TUI, web UI,
	// Cobra CLI, and the binaries.
	LayerPresentation
)

// String names the layer for test failure messages.
func (l Layer) String() string {
	switch l {
	case LayerFoundation:
		return "foundation"
	case LayerTransport:
		return "transport"
	case LayerDomain:
		return "domain"
	case LayerIntelligence:
		return "intelligence"
	case LayerApplication:
		return "application"
	case LayerPresentation:
		return "presentation"
	default:
		return "unknown"
	}
}

// Layers maps every first-party package (path relative to the module root) to
// its layer.
//
// This map is deliberately exhaustive rather than prefix-matched: TestLayering
// fails on any package missing from it, so adding a package forces a conscious
// decision about where it sits. That failure is the point — it is cheaper to
// answer "which layer is this?" while writing the package than to discover the
// answer later from a cycle.
//
// One caveat: the package walk reads only non-test files, so a test-only
// package (cmd/clientsmoke — every file is a build-tagged _test.go) is
// invisible to it. Such an entry is kept honest by the directory-existence
// check, not by import analysis.
var Layers = map[string]Layer{
	// ── Foundation ──
	"internal/arch":                 LayerFoundation, // this package: the rules themselves
	"internal/paths":                LayerFoundation,
	"internal/ignore":               LayerFoundation, // gitignore matching, shared by every walk
	"internal/fsync":                LayerFoundation,
	"internal/sqlitex":              LayerFoundation, // the one place a SQLite DSN is built
	"internal/tokenise":             LayerFoundation,
	"internal/textfmt":              LayerFoundation, // stdlib-only text primitives, no lipgloss
	"internal/toolerror":            LayerFoundation, // stdlib-only structured tool-error contract
	"internal/redact":               LayerFoundation,
	"internal/theme":                LayerFoundation, // UI-agnostic hex palettes, no lipgloss
	"internal/render":               LayerFoundation, // pure presentation helpers
	"internal/quality":              LayerFoundation, // post-write analyser interface
	"internal/quality/golangcilint": LayerFoundation,
	"internal/clientcaps":           LayerFoundation, // static client capability data
	"internal/clienttemplates":      LayerFoundation, // shared per-client instruction template bodies (embedded)

	// ── Transport ──
	"internal/mcp":                     LayerTransport,
	"internal/lsp":                     LayerTransport,
	"internal/lsp/protocol":            LayerTransport,
	"internal/lsp/jsonrpc":             LayerTransport,
	"internal/lsp/watcher":             LayerTransport,
	"internal/lsp/lsptest":             LayerTransport,
	"internal/lsp/conformance":         LayerTransport,
	"internal/lsp/adapters/base":       LayerTransport,
	"internal/lsp/adapters/gopls":      LayerTransport,
	"internal/lsp/adapters/pyright":    LayerTransport,
	"internal/lsp/adapters/jdtls":      LayerTransport,
	"internal/lsp/adapters/rust":       LayerTransport,
	"internal/lsp/adapters/swift":      LayerTransport,
	"internal/lsp/adapters/zig":        LayerTransport,
	"internal/lsp/adapters/typescript": LayerTransport,
	"internal/lsp/adapters/kotlin":     LayerTransport,
	"internal/lsp/adapters/html":       LayerTransport,

	// ── Domain ──
	"internal/config":       LayerDomain,
	"internal/session":      LayerDomain,
	"internal/sessionstate": LayerDomain,
	"internal/stats":        LayerDomain,
	"internal/memory":       LayerDomain,
	"internal/collab":       LayerDomain,
	"internal/cache":        LayerDomain,
	"internal/fsguard":      LayerDomain,
	"internal/langsupport":  LayerDomain,
	"internal/minchange":    LayerDomain,
	"internal/semantics":    LayerDomain,
	"internal/monitor":      LayerDomain,
	"internal/xcodebsp":     LayerDomain,
	"internal/setup":        LayerDomain, // managed instruction block mechanism (markers, idempotent write, drift check)

	// ── Intelligence ──
	"internal/topology":                       LayerIntelligence,
	"internal/topology/extractors/golang":     LayerIntelligence,
	"internal/topology/extractors/treesitter": LayerIntelligence,
	"internal/topology/extractors/typescript": LayerIntelligence,
	"internal/topology/extractors/wasmts":     LayerIntelligence,

	// ── Application ──
	"internal/tools":       LayerApplication,
	"internal/tools/txlog": LayerApplication,

	// ── Presentation ──
	"internal/tui":    LayerPresentation,
	"internal/web":    LayerPresentation,
	"internal/cli":    LayerPresentation,
	"cmd/plumb":       LayerPresentation,
	"cmd/smoke":       LayerPresentation,
	"cmd/clientsmoke": LayerPresentation,
}
