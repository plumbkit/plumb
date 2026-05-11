package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var explainSymbolSchema = json.RawMessage(`{
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

// ExplainSymbol returns hover information (documentation, type signature) for
// the symbol at a given position.
type ExplainSymbol struct {
	client lsp.LSPClient
	cache  *cache.Cache
	ttl    time.Duration
}

// NewExplainSymbol creates an ExplainSymbol tool. Pass a nil cache to disable caching.
func NewExplainSymbol(client lsp.LSPClient, c *cache.Cache, ttl time.Duration) *ExplainSymbol {
	return &ExplainSymbol{client: client, cache: c, ttl: ttl}
}

func (t *ExplainSymbol) Name() string             { return "explain_symbol" }
func (t *ExplainSymbol) InputSchema() json.RawMessage { return explainSymbolSchema }
func (t *ExplainSymbol) Description() string {
	return "Get documentation and type information for the symbol at the given position in a document (hover information from the language server)."
}

type explainSymbolArgs struct {
	URI       string `json:"uri"`
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

func (t *ExplainSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a explainSymbolArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("explain_symbol: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return "", fmt.Errorf("explain_symbol: uri must not be empty")
	}

	key := fmt.Sprintf("%s:hover:%d:%d", a.URI, a.Line, a.Character)
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.(string), nil
		}
	}

	hover, err := t.client.Hover(ctx, protocol.HoverParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: a.Line, Character: a.Character},
	})
	if err != nil {
		return "", positionErr("explain_symbol", err)
	}

	var result string
	if hover == nil || hover.Contents.Value == "" {
		result = fmt.Sprintf("No documentation found for symbol at %s:%d:%d.",
			a.URI, a.Line+1, a.Character+1)
	} else {
		result = hover.Contents.Value
	}

	if t.cache != nil {
		t.cache.Set(key, result, t.ttl)
	}
	return result, nil
}
