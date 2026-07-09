package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// ─── safe_delete_symbol ────────────────────────────────────────────────────

type SafeDeleteSymbol struct {
	client   lsp.Client
	timeout  time.Duration
	ws       WorkspaceFn  // may be nil; anchors a workspace-relative uri to the pinned root
	cache    *cache.Cache // may be nil; evicted after a successful apply so the next query sees fresh symbols
	showDiff func() bool  // may be nil; resolves the show_write_diff toggle (defaults on)
	deps     WriteDeps
	hasDeps  bool
}

func NewSafeDeleteSymbol(client lsp.Client, timeout time.Duration) *SafeDeleteSymbol {
	return &SafeDeleteSymbol{client: client, timeout: timeout}
}

// WithCache wires the session symbol cache so a successful apply evicts uri's
// entries (parity with edit_file/write_file). Nil-safe; returns the tool.
func (t *SafeDeleteSymbol) WithCache(c *cache.Cache) *SafeDeleteSymbol {
	t.cache = c
	return t
}

// WithWorkspace anchors a relative input uri to the pinned workspace. Nil-safe.
func (t *SafeDeleteSymbol) WithWorkspace(ws WorkspaceFn) *SafeDeleteSymbol {
	t.ws = ws
	return t
}

// WithShowWriteDiff wires the per-session show_write_diff resolver. Nil-safe.
func (t *SafeDeleteSymbol) WithShowWriteDiff(fn func() bool) *SafeDeleteSymbol {
	t.showDiff = fn
	return t
}

func (*SafeDeleteSymbol) Name() string { return "safe_delete_symbol" }

func (*SafeDeleteSymbol) Description() string {
	return `Delete a symbol's declaration only if it has no remaining references.

Calls LSP textDocument/references first. If any reference outside the declaration itself is found, the deletion is rejected with the list of referencing locations so the caller can decide what to do. This prevents accidental deletion of code that's still in use.

Set include_doc_comment=true to also delete any contiguous doc comment above the symbol — otherwise the comment is left orphaned, pointing at whatever ends up next in the file.

The response includes a unified diff of the deletion — a preview in dry-run, the applied change otherwise — unless show_write_diff is disabled.`
}

func (*SafeDeleteSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + symbolEditCommonSchema +
		docCommentSchemaFragment +
		`},"required":["uri","name_path"],"additionalProperties":false}`)
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
	a.URI = toFileURIAnchored(a.URI, t.ws)
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	return applySingleEdit(ctx, t.client, t.cache, writeDepsPtr(t.hasDeps, &t.deps), a.URI, dryRun, resolveShowDiff(t.showDiff), "delete", t.Name(), a.DirtyOK, func(ctx context.Context) (protocol.TextEdit, *protocol.DocumentSymbol, string, error) {
		sym, err := resolveSymbol(ctx, t.client, a.URI, a.NamePath)
		if err != nil {
			return protocol.TextEdit{}, nil, "", err
		}

		// Probe references at the symbol's selection range start while the file is
		// still locked for the eventual delete, so the safety check and delete
		// apply to one coherent on-disk version.
		refs, err := t.client.References(ctx, protocol.ReferenceParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
			Position:     sym.SelectionRange.Start,
			Context:      protocol.ReferenceContext{IncludeDeclaration: false},
		})
		if err != nil {
			return protocol.TextEdit{}, nil, "", lspTimeoutErr("safe_delete_symbol", t.timeout, fmt.Errorf("references: %w", err))
		}
		external := 0
		var refLines []string
		for _, r := range refs {
			// Filter out references inside the symbol's own range.
			if r.URI == a.URI && rangeContains(sym.Range, r.Range) {
				continue
			}
			external++
			path := paths.URIToPath(r.URI)
			refLines = append(refLines, fmt.Sprintf("  %s:%d", path, r.Range.Start.Line+1))
		}
		if external > 0 {
			var sb strings.Builder
			fmt.Fprintf(&sb, "REFUSED — symbol %q has %d external reference(s):\n\n", sym.Name, external)
			for _, l := range refLines {
				sb.WriteString(l)
				sb.WriteByte('\n')
			}
			sb.WriteString("\nDelete each reference first, or use replace_symbol_body to keep the symbol but change its content.")
			return protocol.TextEdit{}, nil, "", symbolEditRefusal{msg: sb.String()}
		}

		rng := sym.Range
		if a.IncludeDocComment {
			rng.Start = docCommentStart(paths.URIToPath(a.URI), sym.Range.Start)
		}
		return protocol.TextEdit{Range: rng, NewText: ""}, sym, "", nil
	})
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
