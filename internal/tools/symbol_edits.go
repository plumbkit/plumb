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

	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// Symbol-edit tools share three steps:
//   1. Resolve the target symbol (DocumentSymbol tree → matching name path).
//   2. Compute a single TextEdit at one of the symbol's positions
//      (Start, End, or full Range).
//   3. Apply the edit (atomic write) unless dry_run.

const symbolEditCommonSchema = `
"uri":{"type":"string","description":"Document URI (file://...)."},
"name_path":{"type":"string","description":"Slash-separated symbol path within the file (e.g. \"ClassName/methodName\", or just \"funcName\" for top-level)."},
"dry_run":{"type":"boolean","default":true,"description":"If true (default), preview only; do not write."}
`

type symbolEditArgs struct {
	URI               string `json:"uri"`
	NamePath          string `json:"name_path"`
	Content           string `json:"content"`
	DryRun            *bool  `json:"dry_run,omitempty"`
	IncludeDocComment bool   `json:"include_doc_comment,omitempty"`
}

// docCommentSchemaFragment is the JSON schema snippet for the include_doc_comment
// flag, shared by the three tools that respect it. Always prefixed with a comma
// — call sites already terminate the previous property without a trailing comma.
const docCommentSchemaFragment = `,"include_doc_comment":{"type":"boolean","default":false,"description":"If true, extend the operation to cover any contiguous comment lines (//, #, /*, *) directly above the symbol declaration. Lets you replace/delete a function together with its doc comment, or insert a new block above an existing doc comment instead of between the comment and its symbol."}`

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

// resolveSymbolOrFallback resolves namePath via the LSP document-symbol tree,
// falling back to a fresh tree-sitter parse (topology) when the language server
// errors. viaFallback reports which path produced the symbol so the caller can
// annotate its output (the fallback range is line-granular, not byte-precise).
// When the LSP fails and no fallback resolves the symbol, the original LSP
// error is returned.
func resolveSymbolOrFallback(ctx context.Context, client lsp.Client, topo topologyStoreFn, uri, namePath string) (sym *protocol.DocumentSymbol, viaFallback bool, err error) {
	sym, lspErr := resolveSymbol(ctx, client, uri, namePath)
	if lspErr == nil {
		return sym, false, nil
	}
	nodes, ok := freshTopologyNodes(ctx, topo, uri)
	if !ok {
		return nil, false, lspErr
	}
	node := topologyNodeByPath(nodes, namePath)
	if node == nil {
		return nil, false, lspErr
	}
	ds := nodeToDocSymbol(*node, fileLines(strings.TrimPrefix(uri, "file://")))
	return &ds, true, nil
}

// resolveSymbol fetches the DocumentSymbol tree for uri and locates namePath.
func resolveSymbol(ctx context.Context, client lsp.Client, uri, namePath string) (*protocol.DocumentSymbol, error) {
	syms, err := client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("language server did not respond in time (it may still be indexing the workspace — retry shortly)")
		}
		return nil, fmt.Errorf("documentSymbols: %w", err)
	}
	sym := findSymbolByPath(syms, namePath)
	if sym == nil {
		return nil, fmt.Errorf("symbol %q not found in %s", namePath, strings.TrimPrefix(uri, "file://"))
	}
	return sym, nil
}

// applySingleEdit runs the standard apply-or-preview flow used by every
// symbol-edit tool. summary is the human-readable verb ("inserted before",
// "replaced", etc.) used in the dry-run / applied output.
func applySingleEdit(uri string, edit protocol.TextEdit, dryRun bool, summary string, sym *protocol.DocumentSymbol, viaFallback bool) (string, error) {
	path := strings.TrimPrefix(uri, "file://")
	var sb strings.Builder
	if viaFallback {
		sb.WriteString("[topology fallback — LSP unavailable; symbol located by tree-sitter, range is line-granular]\n\n")
	}
	if dryRun {
		sb.WriteString("DRY RUN — file not modified.\n\n")
		fmt.Fprintf(&sb, "Would %s symbol %q in %s\n", summary, sym.Name, path)
		fmt.Fprintf(&sb, "  Range: line %d char %d → line %d char %d\n",
			edit.Range.Start.Line, edit.Range.Start.Character,
			edit.Range.End.Line, edit.Range.End.Character)
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
		return sb.String(), nil
	}
	if err := applyTextEditsToFile(path, []protocol.TextEdit{edit}); err != nil {
		return "", fmt.Errorf("applying edit: %w", err)
	}
	fmt.Fprintf(&sb, "%s symbol %q in %s\n", capitalise(summary), sym.Name, path)
	return sb.String(), nil
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ─── insert_before_symbol ──────────────────────────────────────────────────

type InsertBeforeSymbol struct {
	client  lsp.Client
	timeout time.Duration
	topo    topologyStoreFn
}

func NewInsertBeforeSymbol(client lsp.Client, timeout time.Duration) *InsertBeforeSymbol {
	return &InsertBeforeSymbol{client: client, timeout: timeout}
}

// WithTopologyFallback wires the topology index so the tool can resolve the
// target symbol from a fresh tree-sitter parse when the language server is
// unavailable. Nil-safe; returns the tool for chaining.
func (t *InsertBeforeSymbol) WithTopologyFallback(fn topologyStoreFn) *InsertBeforeSymbol {
	t.topo = fn
	return t
}

func (*InsertBeforeSymbol) Name() string { return "insert_before_symbol" }

func (*InsertBeforeSymbol) Description() string {
	return `Insert text immediately before a symbol's declaration.

Useful for adding a new function/method before an existing one, or prepending a doc comment. Locates the symbol via the LSP document symbol tree (no manual line counting). Provide the full text to insert in 'content' — include trailing newline if appropriate.

Set include_doc_comment=true to insert before any existing leading doc comment instead of between the comment and the symbol — useful when adding a new function (with its own doc comment) above a function that already has one.

Works even when the language server is cold or cannot parse the file: it then locates the symbol via a fresh tree-sitter parse (line-granular range, annotated in the output).`
}

func (*InsertBeforeSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + symbolEditCommonSchema +
		`,"content":{"type":"string","description":"Text to insert before the symbol."}` +
		docCommentSchemaFragment +
		`},"required":["uri","name_path","content"]}`)
}

func (t *InsertBeforeSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	var a symbolEditArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.URI == "" || a.NamePath == "" {
		return "", fmt.Errorf("`uri` and `name_path` are required")
	}
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	sym, viaFallback, err := resolveSymbolOrFallback(ctx, t.client, t.topo, a.URI, a.NamePath)
	if err != nil {
		return "", err
	}
	start := sym.Range.Start
	if a.IncludeDocComment {
		start = docCommentStart(strings.TrimPrefix(a.URI, "file://"), sym.Range.Start)
	}
	edit := protocol.TextEdit{
		Range:   protocol.Range{Start: start, End: start},
		NewText: a.Content,
	}
	return applySingleEdit(a.URI, edit, dryRun, "insert before", sym, viaFallback)
}

// ─── insert_after_symbol ───────────────────────────────────────────────────

type InsertAfterSymbol struct {
	client  lsp.Client
	timeout time.Duration
	topo    topologyStoreFn
}

func NewInsertAfterSymbol(client lsp.Client, timeout time.Duration) *InsertAfterSymbol {
	return &InsertAfterSymbol{client: client, timeout: timeout}
}

// WithTopologyFallback wires the topology index for symbol resolution when the
// language server is unavailable. Nil-safe; returns the tool for chaining.
func (t *InsertAfterSymbol) WithTopologyFallback(fn topologyStoreFn) *InsertAfterSymbol {
	t.topo = fn
	return t
}

func (*InsertAfterSymbol) Name() string { return "insert_after_symbol" }

func (*InsertAfterSymbol) Description() string {
	return `Insert text immediately after a symbol's declaration.

Useful for adding a new method to a struct (insert after an existing one), or appending a related helper. Provide the full text to insert in 'content' — include leading newline if appropriate.

Works even when the language server is cold or cannot parse the file: it then locates the symbol via a fresh tree-sitter parse (line-granular range, annotated in the output).`
}

func (*InsertAfterSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + symbolEditCommonSchema +
		`,"content":{"type":"string","description":"Text to insert after the symbol."}` +
		`},"required":["uri","name_path","content"]}`)
}

func (t *InsertAfterSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	var a symbolEditArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.URI == "" || a.NamePath == "" {
		return "", fmt.Errorf("`uri` and `name_path` are required")
	}
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	sym, viaFallback, err := resolveSymbolOrFallback(ctx, t.client, t.topo, a.URI, a.NamePath)
	if err != nil {
		return "", err
	}
	edit := protocol.TextEdit{
		Range:   protocol.Range{Start: sym.Range.End, End: sym.Range.End},
		NewText: a.Content,
	}
	return applySingleEdit(a.URI, edit, dryRun, "insert after", sym, viaFallback)
}

// ─── replace_symbol_body ───────────────────────────────────────────────────

type ReplaceSymbolBody struct {
	client  lsp.Client
	timeout time.Duration
	topo    topologyStoreFn
}

func NewReplaceSymbolBody(client lsp.Client, timeout time.Duration) *ReplaceSymbolBody {
	return &ReplaceSymbolBody{client: client, timeout: timeout}
}

// WithTopologyFallback wires the topology index for symbol resolution when the
// language server is unavailable. Nil-safe; returns the tool for chaining.
func (t *ReplaceSymbolBody) WithTopologyFallback(fn topologyStoreFn) *ReplaceSymbolBody {
	t.topo = fn
	return t
}

func (*ReplaceSymbolBody) Name() string { return "replace_symbol_body" }

func (*ReplaceSymbolBody) Description() string {
	return `No native Claude Code equivalent.

Replace the entire declaration of a symbol with new content.

The replacement spans the symbol's full Range as reported by the LSP — for a function, this is from 'func' keyword through the closing '}'. Provide the complete new declaration (signature + body) in 'content'.

Set include_doc_comment=true to also cover any contiguous doc comment above the symbol — gopls and most LSP servers report the symbol range starting at the declaration keyword, so without this flag the old doc comment is left orphaned. With it on, your 'content' must include the new doc comment too (or the symbol will have none).

Use rename_symbol if you only want to change the symbol's name. Use this tool when changing logic, signature, or both.

Works even when the language server is cold or cannot parse the file: it then locates the symbol via a fresh tree-sitter parse (line-granular range, annotated in the output).`
}

func (*ReplaceSymbolBody) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + symbolEditCommonSchema +
		`,"content":{"type":"string","description":"The full replacement declaration."}` +
		docCommentSchemaFragment +
		`},"required":["uri","name_path","content"]}`)
}

func (t *ReplaceSymbolBody) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	var a symbolEditArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.URI == "" || a.NamePath == "" {
		return "", fmt.Errorf("`uri` and `name_path` are required")
	}
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	sym, viaFallback, err := resolveSymbolOrFallback(ctx, t.client, t.topo, a.URI, a.NamePath)
	if err != nil {
		return "", err
	}
	rng := sym.Range
	if a.IncludeDocComment {
		rng.Start = docCommentStart(strings.TrimPrefix(a.URI, "file://"), sym.Range.Start)
	}
	edit := protocol.TextEdit{
		Range:   rng,
		NewText: a.Content,
	}
	return applySingleEdit(a.URI, edit, dryRun, "replace", sym, viaFallback)
}

// ─── safe_delete_symbol ────────────────────────────────────────────────────

type SafeDeleteSymbol struct {
	client  lsp.Client
	timeout time.Duration
}

func NewSafeDeleteSymbol(client lsp.Client, timeout time.Duration) *SafeDeleteSymbol {
	return &SafeDeleteSymbol{client: client, timeout: timeout}
}

func (*SafeDeleteSymbol) Name() string { return "safe_delete_symbol" }

func (*SafeDeleteSymbol) Description() string {
	return `Delete a symbol's declaration only if it has no remaining references.

Calls LSP textDocument/references first. If any reference outside the declaration itself is found, the deletion is rejected with the list of referencing locations so the caller can decide what to do. This prevents accidental deletion of code that's still in use.

Set include_doc_comment=true to also delete any contiguous doc comment above the symbol — otherwise the comment is left orphaned, pointing at whatever ends up next in the file.`
}

func (*SafeDeleteSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + symbolEditCommonSchema +
		docCommentSchemaFragment +
		`},"required":["uri","name_path"]}`)
}

func (t *SafeDeleteSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	var a symbolEditArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.URI == "" || a.NamePath == "" {
		return "", fmt.Errorf("`uri` and `name_path` are required")
	}
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	sym, err := resolveSymbol(ctx, t.client, a.URI, a.NamePath)
	if err != nil {
		return "", err
	}

	// Probe references at the symbol's selection range start.
	refs, err := t.client.References(ctx, protocol.ReferenceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     sym.SelectionRange.Start,
		Context:      protocol.ReferenceContext{IncludeDeclaration: false},
	})
	if err != nil {
		return "", lspTimeoutErr("safe_delete_symbol", t.timeout, fmt.Errorf("references: %w", err))
	}
	external := 0
	var refLines []string
	for _, r := range refs {
		// Filter out references inside the symbol's own range.
		if r.URI == a.URI && rangeContains(sym.Range, r.Range) {
			continue
		}
		external++
		path := strings.TrimPrefix(r.URI, "file://")
		refLines = append(refLines, fmt.Sprintf("  %s:%d", path, r.Range.Start.Line+1))
	}
	if external > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "REFUSED — symbol %q has %d external reference(s):\n\n", sym.Name, external)
		for _, l := range refLines {
			sb.WriteString(l + "\n")
		}
		sb.WriteString("\nDelete each reference first, or use replace_symbol_body to keep the symbol but change its content.")
		return sb.String(), nil
	}

	rng := sym.Range
	if a.IncludeDocComment {
		rng.Start = docCommentStart(strings.TrimPrefix(a.URI, "file://"), sym.Range.Start)
	}
	edit := protocol.TextEdit{Range: rng, NewText: ""}
	return applySingleEdit(a.URI, edit, dryRun, "delete", sym, false)
}

// rangeContains returns true if outer fully contains inner.
func rangeContains(outer, inner protocol.Range) bool {
	if posLess(inner.Start, outer.Start) {
		return false
	}
	if posLess(outer.End, inner.End) {
		return false
	}
	return true
}

func posLess(a, b protocol.Position) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Character < b.Character
}
