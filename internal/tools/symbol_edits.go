package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// Symbol-edit tools share three steps:
//   1. Resolve the target symbol (DocumentSymbol tree → matching name path).
//   2. Compute a single TextEdit at one of the symbol's positions
//      (Start, End, or full Range).
//   3. Apply the edit (atomic write) unless dry_run.

const symbolEditCommonSchema = `
"uri":{"type":"string","description":"File: absolute path, file:// URI, or workspace-relative."},
"name_path":{"type":"string","description":"Symbol path in the file, e.g. \"Class/method\" or \"func\"."},
"dry_run":{"type":"boolean","default":true,"description":"Preview only, no write (default true)."},
"dirty_ok":{"type":"boolean","default":false,"description":"Allow a file with uncommitted git changes (default false)."}
`

type symbolEditArgs struct {
	URI               string `json:"uri"`
	NamePath          string `json:"name_path"`
	Content           string `json:"content"`
	DryRun            *bool  `json:"dry_run,omitempty"`
	DirtyOK           bool   `json:"dirty_ok,omitempty"`
	IncludeDocComment bool   `json:"include_doc_comment,omitempty"`
}

// docCommentSchemaFragment is the JSON schema snippet for the include_doc_comment
// flag, shared by insert_before_symbol, replace_symbol_body and
// safe_delete_symbol. move_symbol accepts the same flag — and defaults it to
// TRUE — but hand-writes its own description, so a change here does not reach
// it. Always prefixed with a comma — call sites already terminate the previous
// property without a trailing comma.
//
// Only the first two climb above a wrapper: they hold a topologyStoreFn and go
// through docCommentStartPreferTopology, whereas SafeDeleteSymbol has no
// topology fallback and line-scans with docCommentStart, which stops at the
// first non-comment line — a decorator or an export keyword — and so can never
// extend past the declaration.
const docCommentSchemaFragment = `,"include_doc_comment":{"type":"boolean","default":false,"description":"Also cover the contiguous comment lines (//, #, /*, *) directly above the declaration. On a wrapped declaration (ES export, Python @decorator) with a comment above the wrapper, insert_before_symbol and replace_symbol_body extend past the wrapper, so replacement content must repeat the export or decorator; safe_delete_symbol never extends past the declaration."}`

// docCommentStart walks upward from symStart to find the first line of any
// contiguous comment block flush against the symbol. Returns symStart if no
// such block exists or the file can't be read.
//
// A "comment line" is any line whose first non-whitespace characters match
// //, #, /*, or *. This covers Go/Rust/C/Java/JS line comments, Python/shell
// hash comments, and the lines of a JSDoc/JavaDoc /** ... */ block. Blank
// lines terminate the scan — the block must be flush against the declaration.
func docCommentStart(path string, symStart protocol.Position) protocol.Position {
	data, err := os.ReadFile(path)
	if err != nil {
		return symStart
	}
	lines := strings.Split(string(data), "\n")
	if int(symStart.Line) > len(lines) {
		return symStart
	}
	first := int(symStart.Line)
	for i := int(symStart.Line) - 1; i >= 0; i-- {
		trimmed := strings.TrimLeft(lines[i], " \t")
		if !isCommentLine(trimmed) {
			break
		}
		first = i
	}
	if first == int(symStart.Line) {
		return symStart
	}
	if first < 0 || first > math.MaxUint32 {
		return symStart
	}
	return protocol.Position{Line: uint32(first), Character: 0}
}

// docCommentStartPreferTopology resolves the start position of sym's leading doc
// comment, where sym is the symbol the tool ALREADY resolved — from the language
// server or from the fallback. It first looks for the topology node of that
// symbol (topologyNodeOfSymbol: same name, span starting on the symbol's line)
// carrying a byte-precise doc span, which the structural extractors record
// exactly, and falls back to the docCommentStart line-scan heuristic when topology
// is unavailable, no node matches exactly, or the node has no doc span. It never
// resolves the name_path again: a second resolution through another tree could
// answer with a different symbol (a nested class's method of the same name), and
// the edit would then cover that symbol's doc comment instead of the one being
// edited.
func docCommentStartPreferTopology(ctx context.Context, topo topologyStoreFn, uri string, sym *protocol.DocumentSymbol) protocol.Position {
	if pos, ok := topologyDocCommentStart(ctx, topo, uri, sym); ok {
		return pos
	}
	return docCommentStart(paths.URIToPath(uri), sym.Range.Start)
}

// topologyDocCommentStart returns the precise start position of sym's doc
// comment from a fresh topology parse, or ok=false when no node of sym with a doc
// span is found.
func topologyDocCommentStart(ctx context.Context, topo topologyStoreFn, uri string, sym *protocol.DocumentSymbol) (protocol.Position, bool) {
	nodes, ok := freshTopologyNodes(ctx, topo, uri)
	if !ok {
		return protocol.Position{}, false
	}
	node := topologyNodeOfSymbol(nodes, sym)
	if node == nil || !node.HasDocSpan() {
		return protocol.Position{}, false
	}
	content, err := os.ReadFile(paths.URIToPath(uri))
	if err != nil {
		return protocol.Position{}, false
	}
	pos, ok := byteOffsetToPosition(content, node.DocStartByte)
	if !ok {
		return protocol.Position{}, false
	}
	// The byte offset names the comment's first content byte (column 4 for an
	// indented member), but an edit-range start must sit at the START of the
	// comment's LINE: replacement content carries its own indentation, so
	// starting at the real column would double-indent it. Zeroing the column
	// makes this path agree with docCommentStart's line-scan fallback.
	pos.Character = 0
	return pos, true
}

func isCommentLine(trimmed string) bool {
	switch {
	case strings.HasPrefix(trimmed, "//"),
		strings.HasPrefix(trimmed, "#"),
		strings.HasPrefix(trimmed, "/*"),
		strings.HasPrefix(trimmed, "*"):
		return true
	}
	return false
}

// ─── insert_before_symbol ──────────────────────────────────────────────────

type InsertBeforeSymbol struct {
	client    lsp.Client
	timeout   time.Duration
	topo      topologyStoreFn
	warmup    LSPWarmupFn  // may be nil; distinguishes a warming server from an unavailable one in the fallback banner
	ws        WorkspaceFn  // may be nil; anchors a workspace-relative uri to the pinned root
	contested ContestedFn  // may be nil; refuses a relative uri once the pin is contested
	cache     *cache.Cache // may be nil; evicted after a successful apply so the next query sees fresh symbols
	showDiff  func() bool  // may be nil; resolves the show_write_diff toggle (defaults on)
	deps      WriteDeps
	hasDeps   bool
}

func NewInsertBeforeSymbol(client lsp.Client, timeout time.Duration) *InsertBeforeSymbol {
	return &InsertBeforeSymbol{client: client, timeout: timeout}
}

// WithCache wires the session symbol cache so a successful apply evicts uri's
// entries (parity with edit_file/write_file). Nil-safe; returns the tool.
func (t *InsertBeforeSymbol) WithCache(c *cache.Cache) *InsertBeforeSymbol {
	t.cache = c
	return t
}

// WithTopologyFallback wires the topology index so the tool can resolve the
// target symbol from a fresh tree-sitter parse when the language server is
// unavailable. Nil-safe; returns the tool for chaining.
func (t *InsertBeforeSymbol) WithTopologyFallback(fn topologyStoreFn) *InsertBeforeSymbol {
	t.topo = fn
	return t
}

// WithLSPWarmup wires the warm-up probe so the tree-sitter fallback banner says
// "still warming" instead of "LSP unavailable" while the server that owns the
// target file is completing its handshake. Nil-safe; returns the tool.
func (t *InsertBeforeSymbol) WithLSPWarmup(fn LSPWarmupFn) *InsertBeforeSymbol {
	t.warmup = fn
	return t
}

// WithWorkspace anchors a relative input uri to the pinned workspace. Nil-safe.
func (t *InsertBeforeSymbol) WithWorkspace(ws WorkspaceFn) *InsertBeforeSymbol {
	t.ws = ws
	return t
}

// WithContested wires the contested-pin reporter so a RELATIVE uri is refused
// once the pin is contested (issue #182). Nil-safe.
func (t *InsertBeforeSymbol) WithContested(fn ContestedFn) *InsertBeforeSymbol {
	t.contested = fn
	return t
}

// WithShowWriteDiff wires the per-session show_write_diff resolver. Nil-safe.
func (t *InsertBeforeSymbol) WithShowWriteDiff(fn func() bool) *InsertBeforeSymbol {
	t.showDiff = fn
	return t
}

func (*InsertBeforeSymbol) Name() string { return "insert_before_symbol" }

func (*InsertBeforeSymbol) Description() string {
	return `Insert text immediately before a symbol's declaration, located by name_path (no line counting): a new function, say, or a doc comment. End content with a newline. include_doc_comment=true inserts above the symbol's existing doc comment instead of between it and the symbol. Returns a unified diff (a preview in dry-run) unless show_write_diff is off. With a cold or failing language server it locates the symbol by tree-sitter (line-granular, noted in the output).`
}

func (*InsertBeforeSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + symbolEditCommonSchema +
		`,"content":{"type":"string","description":"Text to insert before the symbol."}` +
		docCommentSchemaFragment +
		`},"required":["uri","name_path","content"],"additionalProperties":false}`)
}

func (t *InsertBeforeSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// Only the SERVER ATTEMPT is shortened; ctx keeps the tool's own [lsp_query]
	// bound, so the write path below stays bounded exactly as it always was.
	ctx, lspCtx, cancel, waited := fallbackDeadlines(ctx, t.timeout)
	defer cancel()
	var a symbolEditArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.URI == "" || a.NamePath == "" {
		return "", errors.New("`uri` and `name_path` are required")
	}
	var rerr error
	a.URI, rerr = toFileURIAnchored(ctx, a.URI, t.ws, t.contested)
	if rerr != nil {
		return "", fmt.Errorf("%s: %w", t.Name(), rerr)
	}
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	return applySingleEdit(ctx, t.client, t.cache, writeDepsPtr(t.hasDeps, &t.deps), a.URI, dryRun, resolveShowDiff(t.showDiff), "insert before", t.Name(), a.DirtyOK, func(ctx context.Context) (protocol.TextEdit, *protocol.DocumentSymbol, string, error) {
		sym, reason, err := resolveSymbolOrFallback(ctx, lspCtx, t.client, t.topo, t.warmup, a.URI, a.NamePath)
		if err != nil {
			return protocol.TextEdit{}, nil, "", err
		}
		start := sym.Range.Start
		if a.IncludeDocComment {
			start = docCommentStartPreferTopology(ctx, t.topo, a.URI, sym)
		}
		return protocol.TextEdit{
			Range:   protocol.Range{Start: start, End: start},
			NewText: a.Content,
		}, sym, symbolEditFallbackNote(reason, t.warmup, a.URI, waited), nil
	})
}

// ─── insert_after_symbol ───────────────────────────────────────────────────

type InsertAfterSymbol struct {
	client    lsp.Client
	timeout   time.Duration
	topo      topologyStoreFn
	warmup    LSPWarmupFn  // may be nil; distinguishes a warming server from an unavailable one in the fallback banner
	ws        WorkspaceFn  // may be nil; anchors a workspace-relative uri to the pinned root
	contested ContestedFn  // may be nil; refuses a relative uri once the pin is contested
	cache     *cache.Cache // may be nil; evicted after a successful apply so the next query sees fresh symbols
	showDiff  func() bool  // may be nil; resolves the show_write_diff toggle (defaults on)
	deps      WriteDeps
	hasDeps   bool
}

func NewInsertAfterSymbol(client lsp.Client, timeout time.Duration) *InsertAfterSymbol {
	return &InsertAfterSymbol{client: client, timeout: timeout}
}

// WithCache wires the session symbol cache so a successful apply evicts uri's
// entries (parity with edit_file/write_file). Nil-safe; returns the tool.
func (t *InsertAfterSymbol) WithCache(c *cache.Cache) *InsertAfterSymbol {
	t.cache = c
	return t
}

// WithTopologyFallback wires the topology index for symbol resolution when the
// language server is unavailable. Nil-safe; returns the tool for chaining.
func (t *InsertAfterSymbol) WithTopologyFallback(fn topologyStoreFn) *InsertAfterSymbol {
	t.topo = fn
	return t
}

// WithLSPWarmup wires the warm-up probe so the tree-sitter fallback banner says
// "still warming" instead of "LSP unavailable" while the server that owns the
// target file is completing its handshake. Nil-safe; returns the tool.
func (t *InsertAfterSymbol) WithLSPWarmup(fn LSPWarmupFn) *InsertAfterSymbol {
	t.warmup = fn
	return t
}

// WithWorkspace anchors a relative input uri to the pinned workspace. Nil-safe.
func (t *InsertAfterSymbol) WithWorkspace(ws WorkspaceFn) *InsertAfterSymbol {
	t.ws = ws
	return t
}

// WithContested wires the contested-pin reporter so a RELATIVE uri is refused
// once the pin is contested (issue #182). Nil-safe.
func (t *InsertAfterSymbol) WithContested(fn ContestedFn) *InsertAfterSymbol {
	t.contested = fn
	return t
}

// WithShowWriteDiff wires the per-session show_write_diff resolver. Nil-safe.
func (t *InsertAfterSymbol) WithShowWriteDiff(fn func() bool) *InsertAfterSymbol {
	t.showDiff = fn
	return t
}

func (*InsertAfterSymbol) Name() string { return "insert_after_symbol" }

func (*InsertAfterSymbol) Description() string {
	return `Insert text immediately after a symbol's declaration, located by name_path: a new method after an existing one, say. Start content with a newline. Returns a unified diff (a preview in dry-run) unless show_write_diff is off. With a cold or failing language server it locates the symbol by tree-sitter (line-granular, noted in the output).`
}

func (*InsertAfterSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + symbolEditCommonSchema +
		`,"content":{"type":"string","description":"Text to insert after the symbol."}` +
		`},"required":["uri","name_path","content"],"additionalProperties":false}`)
}

func (t *InsertAfterSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// Only the SERVER ATTEMPT is shortened; ctx keeps the tool's own [lsp_query]
	// bound, so the write path below stays bounded exactly as it always was.
	ctx, lspCtx, cancel, waited := fallbackDeadlines(ctx, t.timeout)
	defer cancel()
	var a symbolEditArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.URI == "" || a.NamePath == "" {
		return "", errors.New("`uri` and `name_path` are required")
	}
	var rerr error
	a.URI, rerr = toFileURIAnchored(ctx, a.URI, t.ws, t.contested)
	if rerr != nil {
		return "", fmt.Errorf("%s: %w", t.Name(), rerr)
	}
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	return applySingleEdit(ctx, t.client, t.cache, writeDepsPtr(t.hasDeps, &t.deps), a.URI, dryRun, resolveShowDiff(t.showDiff), "insert after", t.Name(), a.DirtyOK, func(ctx context.Context) (protocol.TextEdit, *protocol.DocumentSymbol, string, error) {
		sym, reason, err := resolveSymbolOrFallback(ctx, lspCtx, t.client, t.topo, t.warmup, a.URI, a.NamePath)
		if err != nil {
			return protocol.TextEdit{}, nil, "", err
		}
		return protocol.TextEdit{
			Range:   protocol.Range{Start: sym.Range.End, End: sym.Range.End},
			NewText: a.Content,
		}, sym, symbolEditFallbackNote(reason, t.warmup, a.URI, waited), nil
	})
}

// ─── replace_symbol_body ───────────────────────────────────────────────────

type ReplaceSymbolBody struct {
	client    lsp.Client
	timeout   time.Duration
	topo      topologyStoreFn
	warmup    LSPWarmupFn  // may be nil; distinguishes a warming server from an unavailable one in the fallback banner
	ws        WorkspaceFn  // may be nil; anchors a workspace-relative uri to the pinned root
	contested ContestedFn  // may be nil; refuses a relative uri once the pin is contested
	cache     *cache.Cache // may be nil; evicted after a successful apply so the next query sees fresh symbols
	showDiff  func() bool  // may be nil; resolves the show_write_diff toggle (defaults on)
	deps      WriteDeps
	hasDeps   bool
}

func NewReplaceSymbolBody(client lsp.Client, timeout time.Duration) *ReplaceSymbolBody {
	return &ReplaceSymbolBody{client: client, timeout: timeout}
}

// WithCache wires the session symbol cache so a successful apply evicts uri's
// entries (parity with edit_file/write_file). Nil-safe; returns the tool.
func (t *ReplaceSymbolBody) WithCache(c *cache.Cache) *ReplaceSymbolBody {
	t.cache = c
	return t
}

// WithTopologyFallback wires the topology index for symbol resolution when the
// language server is unavailable. Nil-safe; returns the tool for chaining.
func (t *ReplaceSymbolBody) WithTopologyFallback(fn topologyStoreFn) *ReplaceSymbolBody {
	t.topo = fn
	return t
}

// WithLSPWarmup wires the warm-up probe so the tree-sitter fallback banner says
// "still warming" instead of "LSP unavailable" while the server that owns the
// target file is completing its handshake. Nil-safe; returns the tool.
func (t *ReplaceSymbolBody) WithLSPWarmup(fn LSPWarmupFn) *ReplaceSymbolBody {
	t.warmup = fn
	return t
}

// WithWorkspace anchors a relative input uri to the pinned workspace. Nil-safe.
func (t *ReplaceSymbolBody) WithWorkspace(ws WorkspaceFn) *ReplaceSymbolBody {
	t.ws = ws
	return t
}

// WithContested wires the contested-pin reporter so a RELATIVE uri is refused
// once the pin is contested (issue #182). Nil-safe.
func (t *ReplaceSymbolBody) WithContested(fn ContestedFn) *ReplaceSymbolBody {
	t.contested = fn
	return t
}

// WithShowWriteDiff wires the per-session show_write_diff resolver. Nil-safe.
func (t *ReplaceSymbolBody) WithShowWriteDiff(fn func() bool) *ReplaceSymbolBody {
	t.showDiff = fn
	return t
}

func (*ReplaceSymbolBody) Name() string { return "replace_symbol_body" }

func (*ReplaceSymbolBody) Description() string {
	return `Replace a symbol's whole declaration (signature and body, e.g. 'func' through the closing '}') with content, located by name_path, with no coordinates to compute. With include_doc_comment=true the range also covers the doc comment above, so content must carry the new comment; without it the old comment stays. To change only the name, use rename_symbol. Returns a unified diff (a preview in dry-run) unless show_write_diff is off. With a cold or failing language server it locates the symbol by tree-sitter (line-granular, noted in the output).`
}

func (*ReplaceSymbolBody) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + symbolEditCommonSchema +
		`,"content":{"type":"string","description":"The full replacement declaration."}` +
		docCommentSchemaFragment +
		`},"required":["uri","name_path","content"],"additionalProperties":false}`)
}

func (t *ReplaceSymbolBody) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// Only the SERVER ATTEMPT is shortened; ctx keeps the tool's own [lsp_query]
	// bound, so the write path below stays bounded exactly as it always was.
	ctx, lspCtx, cancel, waited := fallbackDeadlines(ctx, t.timeout)
	defer cancel()
	var a symbolEditArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.URI == "" || a.NamePath == "" {
		return "", errors.New("`uri` and `name_path` are required")
	}
	var rerr error
	a.URI, rerr = toFileURIAnchored(ctx, a.URI, t.ws, t.contested)
	if rerr != nil {
		return "", fmt.Errorf("%s: %w", t.Name(), rerr)
	}
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	return applySingleEdit(ctx, t.client, t.cache, writeDepsPtr(t.hasDeps, &t.deps), a.URI, dryRun, resolveShowDiff(t.showDiff), "replace", t.Name(), a.DirtyOK, func(ctx context.Context) (protocol.TextEdit, *protocol.DocumentSymbol, string, error) {
		sym, reason, err := resolveSymbolOrFallback(ctx, lspCtx, t.client, t.topo, t.warmup, a.URI, a.NamePath)
		if err != nil {
			return protocol.TextEdit{}, nil, "", err
		}
		rng := sym.Range
		if a.IncludeDocComment {
			rng.Start = docCommentStartPreferTopology(ctx, t.topo, a.URI, sym)
		}
		return protocol.TextEdit{
			Range:   rng,
			NewText: a.Content,
		}, sym, symbolEditFallbackNote(reason, t.warmup, a.URI, waited), nil
	})
}
