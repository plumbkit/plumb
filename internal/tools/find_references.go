package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var findReferencesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "file:// URI of the document containing the symbol"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number of the symbol"
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset of the symbol"
    },
    "include_declaration": {
      "type": "boolean",
      "description": "Include the symbol's own declaration in results (default true)"
    }
  },
  "required": ["uri", "line", "character"]
}`)

// FindReferences returns all usages of a symbol across the workspace.
// Each result includes the source line text so callers do not need to
// open each referenced file separately.
//
// Concurrency: Execute is safe for concurrent use.
type FindReferences struct {
	client lsp.LSPClient
}

func NewFindReferences(client lsp.LSPClient) *FindReferences {
	return &FindReferences{client: client}
}

func (t *FindReferences) Name() string             { return "find_references" }
func (t *FindReferences) InputSchema() json.RawMessage { return findReferencesSchema }
func (t *FindReferences) Description() string {
	return "Find all references to a symbol across the entire workspace. " +
		"Returns file path, line number, and the source line at each reference site, " +
		"so no additional file reads are needed."
}

type findReferencesArgs struct {
	URI                string `json:"uri"`
	Line               uint32 `json:"line"`
	Character          uint32 `json:"character"`
	IncludeDeclaration *bool  `json:"include_declaration"`
}

func (t *FindReferences) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a findReferencesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("find_references: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return "", fmt.Errorf("find_references: uri is required")
	}

	includeDecl := true
	if a.IncludeDeclaration != nil {
		includeDecl = *a.IncludeDeclaration
	}

	// Send didOpen first so gopls has the file in its in-memory view. Without
	// this, references against an unedited file may return positions based
	// on a stale on-disk snapshot — the user sees shifted line numbers and
	// the line preview from readFileLines doesn't contain the symbol.
	openFileForRefs(ctx, t.client, a.URI)

	locs, err := t.client.References(ctx, protocol.ReferenceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: a.Line, Character: a.Character},
		Context:      protocol.ReferenceContext{IncludeDeclaration: includeDecl},
	})
	if err != nil {
		return "", positionErr("find_references", err)
	}
	if len(locs) == 0 {
		return fmt.Sprintf("No references found for symbol at %s:%d:%d.", a.URI, a.Line+1, a.Character+1), nil
	}

	// Group locations by file so each file is read at most once.
	byFile := make(map[string][]protocol.Location)
	for _, loc := range locs {
		byFile[loc.URI] = append(byFile[loc.URI], loc)
	}

	lineTexts := make(map[string]map[uint32]string) // uri → line → text
	for uri, fileLocs := range byFile {
		path := strings.TrimPrefix(uri, "file://")
		needed := make(map[uint32]bool)
		for _, l := range fileLocs {
			needed[l.Range.Start.Line] = true
		}
		lineTexts[uri] = readFileLines(path, needed)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d reference(s) to symbol at %s:%d:%d\n\n",
		len(locs), a.URI, a.Line+1, a.Character+1)

	for _, loc := range locs {
		l := loc.Range.Start.Line
		col := loc.Range.Start.Character
		path := strings.TrimPrefix(loc.URI, "file://")
		lineText := ""
		if m, ok := lineTexts[loc.URI]; ok {
			lineText = "\t" + strings.TrimLeft(m[l], " \t")
		}
		fmt.Fprintf(&sb, "%s:%d:%d%s\n", path, l+1, col+1, lineText)
	}
	return sb.String(), nil
}

// openFileForRefs reads a file and sends textDocument/didOpen so gopls
// builds its in-memory view before we query references. Best-effort: any
// I/O or LSP error is ignored — the subsequent references call will just
// see whatever gopls already had cached.
func openFileForRefs(ctx context.Context, client lsp.LSPClient, uri string) {
	path := strings.TrimPrefix(uri, "file://")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = client.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri,
			LanguageID: "go",
			Version:    1,
			Text:       string(data),
		},
	})
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
