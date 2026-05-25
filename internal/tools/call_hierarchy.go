package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var callHierarchySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Absolute path or file:// URI containing the symbol"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number of the symbol"
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset within the line"
    },
    "direction": {
      "type": "string",
      "enum": ["incoming", "outgoing", "both"],
      "description": "Which call direction to return: callers (incoming), callees (outgoing), or both. Defaults to both."
    }
  },
  "required": ["uri", "line", "character"],
  "additionalProperties": false
}`)

// CallHierarchy implements the call_hierarchy MCP tool.
type CallHierarchy struct {
	client  lsp.Client
	timeout time.Duration
}

// NewCallHierarchy creates a CallHierarchy tool.
func NewCallHierarchy(client lsp.Client, timeout time.Duration) *CallHierarchy {
	return &CallHierarchy{client: client, timeout: timeout}
}

func (t *CallHierarchy) Name() string                 { return "call_hierarchy" }
func (t *CallHierarchy) InputSchema() json.RawMessage { return callHierarchySchema }
func (t *CallHierarchy) Description() string {
	return "No native Claude Code equivalent. " +
		"Show the call hierarchy for a symbol: who calls it (incoming) and what it calls (outgoing). " +
		"Useful for understanding control flow and assessing the impact of changes."
}

type callHierarchyArgs struct {
	URI       string `json:"uri"`
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
	Direction string `json:"direction,omitempty"`
}

func parseCallHierarchyArgs(raw json.RawMessage) (callHierarchyArgs, error) {
	var a callHierarchyArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("call_hierarchy: invalid arguments: %w", err)
	}
	if a.URI == "" {
		return a, fmt.Errorf("call_hierarchy: uri must not be empty")
	}
	a.URI = toFileURI(a.URI)
	if a.Direction == "" {
		a.Direction = "both"
	}
	return a, nil
}

func (t *CallHierarchy) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := parseCallHierarchyArgs(args)
	if err != nil {
		return "", err
	}

	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	items, err := t.client.PrepareCallHierarchy(ctx, protocol.PrepareCallHierarchyParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: a.Line, Character: a.Character},
	})
	if err != nil {
		return "", positionErr("call_hierarchy", err)
	}
	if len(items) == 0 {
		return "No call hierarchy item found at the given position.", nil
	}

	item := items[0]
	var sb strings.Builder
	fmt.Fprintf(&sb, "Call hierarchy for %s (%s) at %s:%d\n\n",
		item.Name, symbolKindName(item.Kind), item.URI, item.Range.Start.Line+1)

	if a.Direction == "incoming" || a.Direction == "both" {
		incoming, err := t.client.IncomingCalls(ctx, protocol.CallHierarchyIncomingCallsParams{Item: item})
		if err != nil {
			return "", lspTimeoutErr("call_hierarchy", t.timeout, fmt.Errorf("incoming: %w", err))
		}
		sb.WriteString("## Callers (incoming)\n\n")
		if len(incoming) == 0 {
			sb.WriteString("  (none)\n")
		} else {
			for _, c := range incoming {
				fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n",
					c.From.Name, symbolKindName(c.From.Kind),
					c.From.URI, c.From.Range.Start.Line+1)
			}
		}
		sb.WriteString("\n")
	}

	if a.Direction == "outgoing" || a.Direction == "both" {
		outgoing, err := t.client.OutgoingCalls(ctx, protocol.CallHierarchyOutgoingCallsParams{Item: item})
		if err != nil {
			return "", lspTimeoutErr("call_hierarchy", t.timeout, fmt.Errorf("outgoing: %w", err))
		}
		sb.WriteString("## Callees (outgoing)\n\n")
		if len(outgoing) == 0 {
			sb.WriteString("  (none)\n")
		} else {
			for _, c := range outgoing {
				fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n",
					c.To.Name, symbolKindName(c.To.Kind),
					c.To.URI, c.To.Range.Start.Line+1)
			}
		}
	}

	return sb.String(), nil
}
