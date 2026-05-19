package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var typeHierarchySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "File URI (file://...) containing the type"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number of the type"
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset within the line"
    },
    "direction": {
      "type": "string",
      "enum": ["supertypes", "subtypes", "both"],
      "description": "Which direction to traverse: parent types (supertypes), child types (subtypes), or both. Defaults to both."
    }
  },
  "required": ["uri", "line", "character"]
}`)

// TypeHierarchy implements the type_hierarchy MCP tool.
type TypeHierarchy struct {
	client lsp.Client
}

// NewTypeHierarchy creates a TypeHierarchy tool.
func NewTypeHierarchy(client lsp.Client) *TypeHierarchy {
	return &TypeHierarchy{client: client}
}

func (t *TypeHierarchy) Name() string                 { return "type_hierarchy" }
func (t *TypeHierarchy) InputSchema() json.RawMessage { return typeHierarchySchema }
func (t *TypeHierarchy) Description() string {
	return "No native Claude Code equivalent. " +
		"Show the type hierarchy for a type: its supertypes (interfaces it implements, embedded types) and subtypes (types that implement or embed it). " +
		"Useful for understanding inheritance and polymorphism."
}

type typeHierarchyArgs struct {
	URI       string `json:"uri"`
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
	Direction string `json:"direction,omitempty"`
}

func (t *TypeHierarchy) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a typeHierarchyArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("type_hierarchy: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return "", fmt.Errorf("type_hierarchy: uri must not be empty")
	}
	if a.Direction == "" {
		a.Direction = "both"
	}

	items, err := t.client.PrepareTypeHierarchy(ctx, protocol.PrepareTypeHierarchyParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: a.Line, Character: a.Character},
	})
	if err != nil {
		return "", positionErr("type_hierarchy", err)
	}
	if len(items) == 0 {
		return "No type hierarchy item found at the given position.", nil
	}

	item := items[0]
	var sb strings.Builder
	fmt.Fprintf(&sb, "Type hierarchy for %s (%s) at %s:%d\n\n",
		item.Name, symbolKindName(item.Kind), item.URI, item.Range.Start.Line+1)

	if a.Direction == "supertypes" || a.Direction == "both" {
		supers, err := t.client.Supertypes(ctx, protocol.TypeHierarchySupertypesParams{Item: item})
		if err != nil {
			return "", fmt.Errorf("type_hierarchy supertypes: %w", err)
		}
		sb.WriteString("## Supertypes\n\n")
		if len(supers) == 0 {
			sb.WriteString("  (none)\n")
		} else {
			for _, s := range supers {
				fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n",
					s.Name, symbolKindName(s.Kind), s.URI, s.Range.Start.Line+1)
			}
		}
		sb.WriteString("\n")
	}

	if a.Direction == "subtypes" || a.Direction == "both" {
		subs, err := t.client.Subtypes(ctx, protocol.TypeHierarchySubtypesParams{Item: item})
		if err != nil {
			return "", fmt.Errorf("type_hierarchy subtypes: %w", err)
		}
		sb.WriteString("## Subtypes\n\n")
		if len(subs) == 0 {
			sb.WriteString("  (none)\n")
		} else {
			for _, s := range subs {
				fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n",
					s.Name, symbolKindName(s.Kind), s.URI, s.Range.Start.Line+1)
			}
		}
	}

	return sb.String(), nil
}
