package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

var findReferencesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the document containing the symbol"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number. Required when symbol_name is not provided."
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset. Required when symbol_name is not provided."
    },
    "symbol_name": {
      "type": "string",
      "description": "Symbol name to look up instead of a position — PREFERRED over line/character. Accepts plain name or ReceiverType.MethodName form. plumb resolves it against the file's symbols, avoiding the off-by-one and 'no identifier found' errors of a hand-computed position. When provided, line and character are not needed."
    },
    "include_declaration": {
      "type": "boolean",
      "description": "Include the symbol's own declaration in results (default true)"
    }
  },
  "required": ["uri"],
  "additionalProperties": false
}`)

// FindReferences returns all usages of a symbol across the workspace.
// Each result includes the source line text so callers do not need to
// open each referenced file separately.
// Accepts either a file position (line+character) or a symbol_name.
//
// Concurrency: Execute is safe for concurrent use.
type FindReferences struct {
	client  lsp.Client
	cache   *cache.Cache
	ttl     time.Duration
	timeout time.Duration
	ws      WorkspaceFn // may be nil; anchors a workspace-relative uri to the pinned root
}

func NewFindReferences(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration) *FindReferences {
	return &FindReferences{client: client, cache: c, ttl: ttl, timeout: timeout}
}

// WithWorkspace anchors a relative uri to the pinned workspace root. Nil-safe.
func (t *FindReferences) WithWorkspace(ws WorkspaceFn) *FindReferences {
	t.ws = ws
	return t
}

func (t *FindReferences) Name() string                 { return "find_references" }
func (t *FindReferences) InputSchema() json.RawMessage { return findReferencesSchema }
func (t *FindReferences) Description() string {
	return "Find all references to a symbol across the entire workspace. " +
		"No native Claude Code equivalent for workspace-wide semantic reference lookup. " +
		"Returns file path, line number, and the source line at each reference site. " +
		"PREFER a name (uri + symbol_name) — plumb resolves the exact identifier position " +
		"for you, avoiding off-by-one errors; a raw file position (uri + line + character) " +
		"is the fallback and, when it lands off an identifier, is snapped to the enclosing symbol."
}

type findReferencesArgs struct {
	URI                string  `json:"uri"`
	Line               *uint32 `json:"line"`
	Character          *uint32 `json:"character"`
	SymbolName         string  `json:"symbol_name"`
	IncludeDeclaration *bool   `json:"include_declaration"`
}

func (t *FindReferences) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a findReferencesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("find_references: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return "", fmt.Errorf("find_references: uri is required")
	}
	a.URI = toFileURIAnchored(a.URI, t.ws)

	includeDecl := true
	if a.IncludeDeclaration != nil {
		includeDecl = *a.IncludeDeclaration
	}

	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	if a.SymbolName != "" {
		return t.executeByName(ctx, a.URI, a.SymbolName, includeDecl)
	}

	if a.Line == nil || a.Character == nil {
		return "", fmt.Errorf("find_references: either symbol_name or both line and character are required")
	}
	return t.executeByPosition(ctx, a.URI, *a.Line, *a.Character, includeDecl)
}

func (t *FindReferences) executeByName(ctx context.Context, uri, name string, includeDecl bool) (string, error) {
	key := uri + ":docSymbols"
	var syms []protocol.DocumentSymbol
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			syms = v.([]protocol.DocumentSymbol)
		}
	}
	if syms == nil {
		var err error
		syms, err = t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err != nil {
			return "", lspTimeoutErr("find_references", t.timeout, fmt.Errorf("resolving symbol %q: %w", name, err))
		}
		if t.cache != nil {
			t.cache.Set(key, syms, t.ttl)
		}
	}

	matches := resolveSymbolsByName(syms, name)
	if len(matches) == 0 {
		return fmt.Sprintf("No symbol named %q in %s.", name, uri), nil
	}

	openFileForRefs(ctx, t.client, uri)

	if len(matches) == 1 {
		sym := matches[0]
		return t.queryReferences(ctx, uri, sym.SelectionRange.Start.Line, sym.SelectionRange.Start.Character, includeDecl, false)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "References for %q (%d symbol matches):\n", name, len(matches))
	for _, sym := range matches {
		fmt.Fprintf(&sb, "\n## %s (%s) line %d\n\n", sym.Name, symbolKindName(sym.Kind), sym.SelectionRange.Start.Line+1)
		result, err := t.queryReferences(ctx, uri, sym.SelectionRange.Start.Line, sym.SelectionRange.Start.Character, includeDecl, false)
		if err != nil {
			fmt.Fprintf(&sb, "(error: %v)\n", err)
			continue
		}
		sb.WriteString(result)
	}
	return sb.String(), nil
}

func (t *FindReferences) executeByPosition(ctx context.Context, uri string, line, character uint32, includeDecl bool) (string, error) {
	openFileForRefs(ctx, t.client, uri)
	return t.queryReferences(ctx, uri, line, character, includeDecl, true)
}

func (t *FindReferences) queryReferences(ctx context.Context, uri string, line, character uint32, includeDecl, allowSnap bool) (string, error) {
	locs, err := t.client.References(ctx, protocol.ReferenceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: line, Character: character},
		Context:      protocol.ReferenceContext{IncludeDeclaration: includeDecl},
	})
	if err != nil {
		if allowSnap && isPositionMissErr(err) {
			return t.snapReferences(ctx, uri, line, character, includeDecl)
		}
		return "", positionErr("find_references", err)
	}
	if len(locs) == 0 {
		return fmt.Sprintf("No references found for symbol at %s:%d:%d.", uri, line+1, character+1), nil
	}

	byFile := make(map[string][]protocol.Location)
	for _, loc := range locs {
		byFile[loc.URI] = append(byFile[loc.URI], loc)
	}

	lineTexts := make(map[string]map[uint32]string)
	for fileURI, fileLocs := range byFile {
		path := paths.URIToPath(fileURI)
		needed := make(map[uint32]bool)
		for _, l := range fileLocs {
			needed[l.Range.Start.Line] = true
		}
		lineTexts[fileURI] = readFileLines(path, needed)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d reference(s) to symbol at %s:%d:%d\n\n",
		len(locs), uri, line+1, character+1)
	for _, loc := range locs {
		l := loc.Range.Start.Line
		col := loc.Range.Start.Character
		path := paths.URIToPath(loc.URI)
		lineText := ""
		if m, ok := lineTexts[loc.URI]; ok {
			lineText = "\t" + strings.TrimLeft(m[l], " \t")
		}
		fmt.Fprintf(&sb, "%s:%d:%d%s\n", path, l+1, col+1, lineText)
	}
	return sb.String(), nil
}

// snapReferences recovers from a raw position that missed an identifier by
// resolving the enclosing document symbol and re-querying references once at its
// SelectionRange.Start. When nothing encloses the line it returns an actionable
// error naming nearby symbols. The retry passes allowSnap=false so a snap can
// never recurse.
func (t *FindReferences) snapReferences(ctx context.Context, uri string, line, character uint32, includeDecl bool) (string, error) {
	snapped, syms, ok := snapPosition(ctx, t.client, uri, line)
	if !ok {
		return "", positionMissErr("find_references", uri, line, syms)
	}
	out, err := t.queryReferences(ctx, uri, snapped.Line, snapped.Character, includeDecl, false)
	if err != nil {
		return "", err
	}
	return snapNotice(uri, line, character, snapped.Line) + out, nil
}

// openFileForRefs reads a file and sends textDocument/didOpen so the language
// server builds its in-memory view before we query references. Best-effort:
// any I/O or LSP error is ignored — the subsequent references call will just
// see whatever the server already had cached.
func openFileForRefs(ctx context.Context, client lsp.Client, uri string) {
	path := paths.URIToPath(uri)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = client.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri,
			LanguageID: languageIDFromPath(path),
			Version:    1,
			Text:       string(data),
		},
	})
}

// languageIDFromPath returns the LSP language identifier for a file based on
// its extension. Falls back to "plaintext" for unrecognised extensions.
func languageIDFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py", ".pyi":
		return "python"
	case ".java":
		return "java"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	default:
		return "plaintext"
	}
}

// readFileLines opens path and returns the text of the requested lines (zero-based).
// Lines not found in the file are silently omitted from the result.
func readFileLines(path string, lines map[uint32]bool) map[uint32]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	result := make(map[uint32]string, len(lines))
	scanner := bufio.NewScanner(f)
	var lineNum uint32
	for scanner.Scan() {
		if lines[lineNum] {
			text := scanner.Text()
			if len(text) > 120 {
				text = text[:120] + "…"
			}
			result[lineNum] = text
		}
		lineNum++
	}
	return result
}
