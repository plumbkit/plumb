package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var getDefinitionSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Document URI (file:// scheme)"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number",
      "minimum": 0
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset",
      "minimum": 0
    }
  },
  "required": ["uri", "line", "character"]
}`)

// GetDefinition returns the definition location(s) for a symbol at a position.
type GetDefinition struct {
	client lsp.LSPClient
	cache  *cache.Cache
	ttl    time.Duration
}

// NewGetDefinition creates a GetDefinition tool. Pass a nil cache to disable caching.
func NewGetDefinition(client lsp.LSPClient, c *cache.Cache, ttl time.Duration) *GetDefinition {
	return &GetDefinition{client: client, cache: c, ttl: ttl}
}

func (t *GetDefinition) Name() string             { return "get_definition" }
func (t *GetDefinition) InputSchema() json.RawMessage { return getDefinitionSchema }
func (t *GetDefinition) Description() string {
	return "Returns the SOURCE LOCATION (file path + line number) of where a symbol is defined. " +
		"Use when you need to navigate to the implementation of a symbol you're looking at. " +
		"For documentation, type signature, or doc comments at the same position, use explain_symbol instead."
}

type getDefinitionArgs struct {
	URI       string `json:"uri"`
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

func (t *GetDefinition) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a getDefinitionArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("get_definition: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return "", fmt.Errorf("get_definition: uri must not be empty")
	}

	key := fmt.Sprintf("%s:def:%d:%d", a.URI, a.Line, a.Character)
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.(string), nil
		}
	}

	locs, err := t.client.Definition(ctx, protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: a.Line, Character: a.Character},
	})
	if err != nil {
		return "", positionErr("get_definition", err)
	}

	var result string
	if len(locs) == 0 {
		result = fmt.Sprintf("No definition found for symbol at %s:%d:%d.",
			a.URI, a.Line+1, a.Character+1)
	} else {
		var sb strings.Builder
		if len(locs) == 1 {
			l := locs[0]
			fmt.Fprintf(&sb, "Definition at %s:%d:%d\n",
				l.URI, l.Range.Start.Line+1, l.Range.Start.Character+1)
		} else {
			fmt.Fprintf(&sb, "%d definitions for symbol at %s:%d:%d:\n\n",
				len(locs), a.URI, a.Line+1, a.Character+1)
			for i, l := range locs {
				fmt.Fprintf(&sb, "%d. %s:%d:%d\n",
					i+1, l.URI, l.Range.Start.Line+1, l.Range.Start.Character+1)
			}
		}
		result = sb.String()
	}

	if t.cache != nil {
		t.cache.Set(key, result, t.ttl)
	}
	return result, nil
}
