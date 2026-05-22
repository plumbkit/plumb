package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/golimpio/plumb/internal/session"
)

// renameSession lets the current MCP session replace its generated display
// name with a short user- or agent-chosen name.
type renameSession struct {
	rename func(string) (string, error)
}

// NewRenameSession creates a tool for renaming the current MCP session.
func NewRenameSession(rename func(string) (string, error)) *renameSession {
	return &renameSession{rename: rename}
}

func (t *renameSession) Name() string { return "rename_session" }

func (t *renameSession) Description() string {
	return fmt.Sprintf(
		"Renames the current MCP session. Names may contain letters (any case), digits, and hyphens, capped at %d characters. User-provided case is preserved; auto-generated names are lowercase.",
		session.MaxNameLength,
	)
}

func (t *renameSession) InputSchema() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "New session name. Letters, digits, and hyphens only; max %d characters. Cannot start/end with hyphen or contain consecutive hyphens. Case is preserved as entered."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`, session.MaxNameLength))
}

func (t *renameSession) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if t.rename == nil {
		return "", fmt.Errorf("session rename is not available")
	}
	name, err := t.rename(a.Name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("session renamed to %s", name), nil
}
