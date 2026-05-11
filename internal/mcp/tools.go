package mcp

import (
	"context"
	"encoding/json"
)

// Tool is implemented by each MCP tool exposed to the LLM client.
// Tools are registered once at startup and must be safe for concurrent calls.
type Tool interface {
	// Name is the unique identifier sent in tools/list and used in tools/call.
	Name() string
	// Description is shown to the LLM when it selects which tool to invoke.
	Description() string
	// InputSchema is the JSON Schema object describing the arguments map.
	InputSchema() json.RawMessage
	// Execute is called when the LLM invokes the tool.
	// Returns human-readable text or an error (reported as isError:true).
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}
