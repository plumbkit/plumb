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
"uri":{"type":"string","description":"Absolute path, file:// URI, or workspace-relative path."},
"name_path":{"type":"string","description":"Slash-separated symbol path within the file (e.g. \"ClassName/methodName\", or just \"funcName\" for top-level)."},
"dry_run":{"type":"boolean","default":true,"description":"If true (default), preview only; do not write."},
"dirty_ok":{"type":"boolean","default":false,"description":"Allow editing a file with uncommitted changes. Default false — review/commit first, or pass true to proceed."}
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
const docCommentSchemaFragment = `,"include_doc_comment":{"type":"boolean","default":false,"description":"If true, extend the operation to cover any contiguous comment lines (//, #, /*, *) directly above the symbol declaration. Lets you replace/delete a function together with its doc comment, or insert a new block above an existing doc comment instead of between the comment and its symbol. A WRAPPED declaration (an exported ES declaration under its export statement, a decorated Python def under its @decorator) keeps its doc comment above the wrapper, so WHEN SUCH A COMMENT EXISTS insert_before_symbol and replace_symbol_body extend past the wrapper — replacement content must then reproduce the export keyword or the decorator, or it is dropped. With no doc comment above the wrapper the range starts at the declaration and the wrapper is untouched; safe_delete_symbol never extends past the declaration at all."}`

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

// docCommentStartPreferTopology resolves the start position of namePath's
// leading doc comment. It first asks the topology index for a node carrying a
// byte-precise doc span (which the structural extractors record exactly), and
// falls back to the docCommentStart line-scan heuristic when topology is
// unavailable, has no matching node, or that node has no doc span. The line-scan
// remains the fallback for the LSP-only path, so this never regresses callers.
func docCommentStartPreferTopology(ctx context.Context, topo topologyStoreFn, uri, namePath string, symStart protocol.Position) protocol.Position {
	path := paths.URIToPath(uri)
	if pos, ok := topologyDocCommentStart(ctx, topo, uri, namePath); ok {
		return pos
	}
	return docCommentStart(path, symStart)
}

// topologyDocCommentStart returns the precise start position of namePath's doc
// comment from a fresh topology parse, or ok=false when no node with a doc span
// resolves.
func topologyDocCommentStart(ctx context.Context, topo topologyStoreFn, uri, namePath string) (protocol.Position, bool) {
	nodes, ok := freshTopologyNodes(ctx, topo, uri)
	if !ok {
		return protocol.Position{}, false
	}
	node := topologyNodeByPath(nodes, namePath)
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

// symbolFallbackReason says why the tree-sitter fallback answered instead of
// the language server. The distinction reaches the agent: a server that is
// ABSENT and one that simply did not answer inside its attempt budget call for
// different responses (give up on the LSP vs. retry once it is warm), and both
// hand back a line-granular range rather than a byte-precise one.
type symbolFallbackReason int

const (
	fallbackNotUsed symbolFallbackReason = iota
	fallbackLSPUnavailable
	fallbackLSPTimedOut
)

// resolveSymbolOrFallback resolves namePath via the LSP document-symbol tree,
// falling back to a fresh tree-sitter parse (topology) when the language server
// errors. The reason reports which path produced the symbol, and why, so the
// caller can annotate its output (the fallback range is line-granular, not
// byte-precise). When the LSP fails and no fallback resolves the symbol, the
// original LSP error is returned.
//
// It takes TWO contexts on purpose. lspCtx bounds the server attempt and is
// spent once that attempt misses its budget; ctx is the caller's live context
// and is what the fallback runs on. Handing the fallback lspCtx — which is what
// every symbol-edit tool used to do — makes it inoperative rather than merely
// late: topology's safeExtract refuses to start a parse on an expired context,
// so the tool surfaces the very timeout the fallback exists to replace
// (PLAN-390, PLAN-403). See withFallbackLSPDeadline.
func resolveSymbolOrFallback(ctx, lspCtx context.Context, client lsp.Client, topo topologyStoreFn, uri, namePath string) (sym *protocol.DocumentSymbol, reason symbolFallbackReason, err error) {
	sym, lspErr := resolveSymbol(lspCtx, client, uri, namePath)
	if lspErr == nil {
		return sym, fallbackNotUsed, nil
	}
	if IsWorkspaceBoundaryError(lspErr) {
		return nil, fallbackNotUsed, lspErr
	}
	nodes, ok := freshTopologyNodes(ctx, topo, uri)
	if !ok {
		return nil, fallbackNotUsed, lspErr
	}
	node := topologyNodeByPath(nodes, namePath)
	if node == nil {
		return nil, fallbackNotUsed, lspErr
	}
	ds := nodeToDocSymbol(*node, fileLines(paths.URIToPath(uri)))
	return &ds, lspFallbackReason(lspCtx), nil
}

// lspFallbackReason classifies a failed server attempt from the attempt context
// itself, so the error text the LSP path returns stays untouched: an expired
// lspCtx means the server was too slow, anything else means it could not answer
// at all.
func lspFallbackReason(lspCtx context.Context) symbolFallbackReason {
	if errors.Is(lspCtx.Err(), context.DeadlineExceeded) {
		return fallbackLSPTimedOut
	}
	return fallbackLSPUnavailable
}

// resolveSymbol fetches the DocumentSymbol tree for uri and locates namePath.
func resolveSymbol(ctx context.Context, client lsp.Client, uri, namePath string) (*protocol.DocumentSymbol, error) {
	syms, err := client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("language server did not respond in time (it may still be indexing the workspace — retry shortly)")
		}
		return nil, fmt.Errorf("documentSymbols: %w", err)
	}
	sym := findSymbolByPath(syms, namePath)
	if sym == nil {
		return nil, fmt.Errorf("symbol %q not found in %s", namePath, paths.URIToPath(uri))
	}
	return sym, nil
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
	return `Insert text immediately before a symbol's declaration.

Useful for adding a new function/method before an existing one, or prepending a doc comment. Locates the symbol via the LSP document symbol tree (no manual line counting). Provide the full text to insert in 'content' — include trailing newline if appropriate.

Set include_doc_comment=true to insert before any existing leading doc comment instead of between the comment and the symbol — useful when adding a new function (with its own doc comment) above a function that already has one.

The response includes a unified diff of the change — a preview in dry-run, the applied change otherwise — unless show_write_diff is disabled.

Works even when the language server is cold or cannot parse the file: it then locates the symbol via a fresh tree-sitter parse (line-granular range, annotated in the output).`
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
		sym, reason, err := resolveSymbolOrFallback(ctx, lspCtx, t.client, t.topo, a.URI, a.NamePath)
		if err != nil {
			return protocol.TextEdit{}, nil, "", err
		}
		start := sym.Range.Start
		if a.IncludeDocComment {
			start = docCommentStartPreferTopology(ctx, t.topo, a.URI, a.NamePath, sym.Range.Start)
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
	return `Insert text immediately after a symbol's declaration.

Useful for adding a new method to a struct (insert after an existing one), or appending a related helper. Provide the full text to insert in 'content' — include leading newline if appropriate.

The response includes a unified diff of the change — a preview in dry-run, the applied change otherwise — unless show_write_diff is disabled.

Works even when the language server is cold or cannot parse the file: it then locates the symbol via a fresh tree-sitter parse (line-granular range, annotated in the output).`
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
		sym, reason, err := resolveSymbolOrFallback(ctx, lspCtx, t.client, t.topo, a.URI, a.NamePath)
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
	return `Replace the entire declaration of a symbol with new content.

The replacement spans the symbol's full Range as reported by the LSP — for a function, this is from 'func' keyword through the closing '}'. Provide the complete new declaration (signature + body) in 'content'.

Set include_doc_comment=true to also cover any contiguous doc comment above the symbol — gopls and most LSP servers report the symbol range starting at the declaration keyword, so without this flag the old doc comment is left orphaned. With it on, your 'content' must include the new doc comment too (or the symbol will have none).

Use rename_symbol if you only want to change the symbol's name. Use this tool when changing logic, signature, or both — addressed by name_path, no line/character coordinates to compute like edit_file's range mode.

The response includes a unified diff of the change — a preview in dry-run, the applied change otherwise — unless show_write_diff is disabled.

Works even when the language server is cold or cannot parse the file: it then locates the symbol via a fresh tree-sitter parse (line-granular range, annotated in the output).`
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
		sym, reason, err := resolveSymbolOrFallback(ctx, lspCtx, t.client, t.topo, a.URI, a.NamePath)
		if err != nil {
			return protocol.TextEdit{}, nil, "", err
		}
		rng := sym.Range
		if a.IncludeDocComment {
			rng.Start = docCommentStartPreferTopology(ctx, t.topo, a.URI, a.NamePath, sym.Range.Start)
		}
		return protocol.TextEdit{
			Range:   rng,
			NewText: a.Content,
		}, sym, symbolEditFallbackNote(reason, t.warmup, a.URI, waited), nil
	})
}
