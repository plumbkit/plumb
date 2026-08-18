package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/plumbkit/plumb/internal/session"
)

// sessionNamePattern is the JSON Schema regex advertised for the name argument.
// It encodes the same charset and hyphen rules as session.NormaliseName
// (letters/digits/hyphens, no leading/trailing or consecutive hyphens); the
// length cap is carried separately by maxLength. NormaliseName remains the
// authoritative server-side validator — the pattern is advisory for clients.
const sessionNamePattern = `^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`

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
		"Renames the current MCP session. Pass the new name as the `name` parameter — letters (any case), digits, and hyphens, capped at %d characters, with no leading/trailing or consecutive hyphens. User-provided case is preserved; auto-generated names are lowercase. The name must be free: a session name is the address the mailbox delivers to, so a name another LIVE session already answers to is refused (compared case-insensitively) and %q is reserved for the next-arrival address. Renaming to the name you already hold is fine.",
		session.MaxNameLength, "next",
	)
}

func (t *renameSession) InputSchema() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "minLength": 1,
      "maxLength": %d,
      "pattern": "%s",
      "description": "New session name. Letters, digits, and hyphens only; max %d characters. Cannot start/end with hyphen or contain consecutive hyphens. Case is preserved as entered. Must not be a name a live session already uses, and must not be 'next'."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`, session.MaxNameLength, sessionNamePattern, session.MaxNameLength))
}

func (t *renameSession) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if t.rename == nil {
		return "", errors.New("session rename is not available")
	}
	name, err := t.rename(a.Name)
	if err != nil {
		if errors.Is(err, session.ErrNameTaken) {
			// Say why it is refused rather than just that it is: an agent that
			// reads "taken" as an arbitrary rule tends to retry the same name.
			return "", fmt.Errorf("%w — a session name is the address the mailbox delivers to, so two live sessions cannot share one; pick another", err)
		}
		return "", err
	}
	return "session renamed to " + name, nil
}
