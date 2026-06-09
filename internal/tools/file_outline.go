package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
)

var fileOutlineSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Absolute path or file:// URI of the file to outline."
    },
    "include_docs": {
      "type": "boolean",
      "description": "Prepend the first line of each symbol's leading doc comment (// /// /** # styles). Default true.",
      "default": true
    }
  },
  "required": ["uri"],
  "additionalProperties": false
}`)

// outlineMaxBytes caps the file size the outline will read whole. A 2 MiB
// source file is already far larger than the skeleton view is meant for.
const outlineMaxBytes = 2 << 20

// FileOutline returns a token-cheap skeleton of a file: every symbol's
// signature line with its body collapsed, nested by containment, byte-precise.
// It is the "show me the shape of this file in one call" tool — a 2000-line
// file becomes a few hundred tokens.
//
// Symbols come from the language server (documentSymbol) when one answers, and
// from the tree-sitter topology index when the server is cold or absent — so
// the outline still works for files no warm LSP covers. Signatures and doc
// comments are read byte-precise from the file in either case.
//
// Concurrency: Execute is safe for concurrent use.
type FileOutline struct {
	client  lsp.Client
	cache   *cache.Cache
	ttl     time.Duration
	timeout time.Duration
	topo    topologyStoreFn
	guard   BoundaryGuard
}

// NewFileOutline constructs the tool. It shares the documentSymbol cache key
// with list_symbols, so a warm outline reuses an existing symbol query.
func NewFileOutline(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration) *FileOutline {
	return &FileOutline{client: client, cache: c, ttl: ttl, timeout: timeout}
}

// WithTopologyFallback wires the topology index as the source when the language
// server errors or times out. Returns the tool for chaining.
func (t *FileOutline) WithTopologyFallback(fn topologyStoreFn) *FileOutline {
	t.topo = fn
	return t
}

func (t *FileOutline) WithBoundary(guard BoundaryGuard) *FileOutline {
	t.guard = guard
	return t
}

func (*FileOutline) Name() string                 { return "file_outline" }
func (*FileOutline) InputSchema() json.RawMessage { return fileOutlineSchema }
func (*FileOutline) Description() string {
	return "Return a token-cheap skeleton of a file: every function, type, method, class, and " +
		"constant as its signature line with the body collapsed, nested by containment, with " +
		"byte-precise 1-based line ranges. Use it to understand a large file's shape in one call " +
		"without reading it — a 2000-line file becomes a few hundred tokens. Symbols come from the " +
		"language server when available, falling back to the tree-sitter topology index when the " +
		"server is cold or does not cover the file (source is annotated). Set include_docs=false to " +
		"omit leading doc-comment lines."
}

type fileOutlineArgs struct {
	URI         string `json:"uri"`
	IncludeDocs *bool  `json:"include_docs"`
}

// outlineEntry is a symbol normalised from either the LSP tree or the topology
// index. Start/End are 1-based; Depth is the containment nesting level.
type outlineEntry struct {
	Depth int
	Name  string
	Kind  string
	Start int
	End   int
}

type outlineResult struct {
	uri         string
	source      string // "lsp" or "topology"
	lines       []string
	entries     []outlineEntry
	includeDocs bool
}

func (t *FileOutline) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseFileOutlineArgs(raw)
	if err != nil {
		return "", err
	}
	res, err := t.run(ctx, a)
	if err != nil {
		return "", err
	}
	return formatFileOutline(res), nil
}

func parseFileOutlineArgs(raw json.RawMessage) (fileOutlineArgs, error) {
	var a fileOutlineArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("file_outline: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return a, fmt.Errorf("file_outline: uri is required")
	}
	a.URI = toFileURI(a.URI)
	return a, nil
}

func (t *FileOutline) run(ctx context.Context, a fileOutlineArgs) (*outlineResult, error) {
	path := strings.TrimPrefix(a.URI, "file://")
	if err := t.guard.check(path); err != nil {
		return nil, fmt.Errorf("file_outline: %w", err)
	}
	lines, err := readSourceLines(path)
	if err != nil {
		return nil, fmt.Errorf("file_outline: %w", err)
	}
	entries, source, err := t.entries(ctx, a.URI)
	if err != nil {
		return nil, err
	}
	includeDocs := a.IncludeDocs == nil || *a.IncludeDocs
	return &outlineResult{uri: a.URI, source: source, lines: lines, entries: entries, includeDocs: includeDocs}, nil
}

// preferStructuralOutline reports whether the file's language prefers the
// topology Map over the LSP for outline-style views (markup like HTML/Markdown,
// whose documentSymbol is too noisy — a node per tag and attribute — to serve
// as an outline). The LSP remains the source for hover/diagnostics.
func preferStructuralOutline(uri string) bool {
	lang, ok := langsupport.ByPath(strings.TrimPrefix(uri, "file://"))
	return ok && lang.PreferStructuralOutline
}

// entries resolves the file's symbols, preferring the language server and
// falling back to the topology index when it errors or times out. For markup
// languages flagged PreferStructuralOutline, the Map is consulted FIRST (the
// LSP outline is unusably noisy), falling through to the LSP only when the Map
// has nothing.
func (t *FileOutline) entries(ctx context.Context, uri string) ([]outlineEntry, string, error) {
	if preferStructuralOutline(uri) {
		if e, ok := t.topologyEntries(ctx, uri); ok {
			return e, "topology", nil
		}
		// Map empty/unavailable — fall through to the LSP path below.
	}
	syms, lspErr := t.lspSymbols(ctx, uri)
	if lspErr == nil {
		var out []outlineEntry
		flattenLSPSymbols(syms, 0, &out)
		if len(out) > 0 {
			return out, "lsp", nil
		}
		// The server answered but found nothing — typically a file type the
		// workspace LSP does not own (an .html in a Go workspace). Fall back to
		// the structural Map, which indexes every language regardless of which
		// LSP is attached.
		if e, ok := t.topologyEntries(ctx, uri); ok {
			return e, "topology", nil
		}
		return out, "lsp", nil
	}
	if IsWorkspaceBoundaryError(lspErr) {
		return nil, "", lspErr
	}
	if e, ok := t.topologyEntries(ctx, uri); ok {
		return e, "topology", nil
	}
	return nil, "", lspTimeoutErr("file_outline", t.timeout, lspErr)
}

func (t *FileOutline) lspSymbols(ctx context.Context, uri string) ([]protocol.DocumentSymbol, error) {
	key := uri + ":docSymbols"
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.([]protocol.DocumentSymbol), nil
		}
	}
	lspCtx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	syms, err := t.client.DocumentSymbols(lspCtx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		return nil, err
	}
	if t.cache != nil {
		t.cache.Set(key, syms, t.ttl)
	}
	return syms, nil
}

func (t *FileOutline) topologyEntries(ctx context.Context, uri string) ([]outlineEntry, bool) {
	store := activeTopology(t.topo)
	if store == nil {
		return nil, false
	}
	nodes, err := store.SymbolsInFile(ctx, uri)
	if err != nil || len(nodes) == 0 {
		return nil, false
	}
	return nestByRange(nodes), true
}

// flattenLSPSymbols walks the documentSymbol tree into depth-tagged entries.
func flattenLSPSymbols(syms []protocol.DocumentSymbol, depth int, out *[]outlineEntry) {
	for _, s := range syms {
		*out = append(*out, outlineEntry{
			Depth: depth,
			Name:  s.Name,
			Kind:  symbolKindName(s.Kind),
			Start: int(s.Range.Start.Line) + 1,
			End:   int(s.Range.End.Line) + 1,
		})
		flattenLSPSymbols(s.Children, depth+1, out)
	}
}

// nestByRange infers containment depth from line spans: a symbol nested wholly
// inside another's range is a child. The topology index returns a flat node
// list, so this reconstructs the tree the LSP path gets for free.
func nestByRange(nodes []topology.Node) []outlineEntry {
	type span struct {
		name       string
		kind       string
		start, end int
	}
	spans := make([]span, 0, len(nodes))
	for _, n := range nodes {
		end := n.EndLine
		if end < n.StartLine {
			end = n.StartLine
		}
		spans = append(spans, span{name: n.Name, kind: string(n.Kind), start: n.StartLine, end: end})
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	out := make([]outlineEntry, 0, len(spans))
	var stack []span // enclosing, multi-line spans only
	for _, s := range spans {
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.start <= s.start && s.end <= top.end && top.end > top.start {
				break
			}
			stack = stack[:len(stack)-1]
		}
		out = append(out, outlineEntry{Depth: len(stack), Name: s.name, Kind: s.kind, Start: s.start, End: s.end})
		if s.end > s.start {
			stack = append(stack, s)
		}
	}
	return out
}

func readSourceLines(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > outlineMaxBytes {
		return nil, fmt.Errorf("file too large for an outline (%d bytes > %d)", info.Size(), outlineMaxBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(b), "\n"), nil
}

func formatFileOutline(res *outlineResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Outline of %s — %d symbols (source=%s)\n", res.uri, len(res.entries), res.source)
	sb.WriteString("Signatures shown; bodies collapsed. Line ranges are 1-based.\n\n")
	if len(res.entries) == 0 {
		sb.WriteString("(no symbols)\n")
		return sb.String()
	}
	for _, e := range res.entries {
		indent := strings.Repeat("  ", e.Depth)
		if res.includeDocs {
			if doc := docCommentAbove(res.lines, e.Start); doc != "" {
				fmt.Fprintf(&sb, "%s· %s\n", indent, doc)
			}
		}
		sig := signatureAt(res.lines, e.Start)
		if sig == "" {
			sig = e.Name
		}
		fmt.Fprintf(&sb, "%s%s  [%s %s]\n", indent, sig, e.Kind, lineSpan(e.Start, e.End))
	}
	return sb.String()
}

func lineSpan(start, end int) string {
	if end > start {
		return fmt.Sprintf("L%d-%d", start, end)
	}
	return fmt.Sprintf("L%d", start)
}

// signatureAt returns the declaration at the symbol's start line with the body
// stripped: the first non-blank line, extended across continuation lines while
// parentheses stay unbalanced (multi-line signatures), truncated at the body
// opener `{`.
func signatureAt(lines []string, start int) string {
	idx := start - 1
	for idx >= 0 && idx < len(lines) && strings.TrimSpace(lines[idx]) == "" {
		idx++
	}
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	sig := strings.TrimSpace(lines[idx])
	open := parenBalance(sig)
	for n := 1; open > 0 && idx+n < len(lines) && n < 4; n++ {
		next := strings.TrimSpace(lines[idx+n])
		sig += " " + next
		open += parenBalance(next)
	}
	if i := strings.IndexByte(sig, '{'); i >= 0 {
		sig = strings.TrimSpace(sig[:i])
	}
	if len(sig) > 160 {
		sig = sig[:160] + "…"
	}
	return sig
}

func parenBalance(s string) int {
	return strings.Count(s, "(") - strings.Count(s, ")")
}

// docCommentAbove returns the first line of a contiguous comment block directly
// above the symbol (// /// /** * # styles), with its marker stripped. Returns
// "" when the preceding line is not a comment.
func docCommentAbove(lines []string, start int) string {
	idx := start - 2 // line directly above the declaration (0-based)
	var block []string
	for idx >= 0 {
		s := strings.TrimSpace(lines[idx])
		if !isCommentLine(s) {
			break
		}
		block = append(block, s)
		idx--
	}
	if len(block) == 0 {
		return ""
	}
	top := stripCommentMarker(block[len(block)-1]) // topmost line of the block
	if len(top) > 100 {
		top = top[:100] + "…"
	}
	return top
}

func stripCommentMarker(s string) string {
	for _, p := range []string{"///", "/**", "/*", "//", "*/", "*", "#"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	return strings.TrimSpace(strings.TrimSuffix(s, "*/"))
}
