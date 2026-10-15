package tools

// tool_feedback.go — tool_feedback: an agent's short note on what a plumb answer
// lacked or carried needlessly. The tool itself stores nothing: plumb's call
// record keeps every call's arguments, and `plumb stats --context` reads these
// notes back as leads. That makes the note exactly as durable, as private and as
// bounded as the rest of the call record, with no second store to secure.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// toolFeedbackMaxBytes bounds each text field: a sentence or two, not a log.
const toolFeedbackMaxBytes = 500

var toolFeedbackSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "tool": {"type": "string", "description": "The plumb tool the note is about."},
    "missing": {"type": "string", "description": "What you needed and had to find another way."},
    "noise": {"type": "string", "description": "What it gave you that you did not need."},
    "note": {"type": "string", "description": "Anything else."}
  },
  "required": ["tool"],
  "additionalProperties": false
}`)

// ToolFeedback is the tool_feedback MCP tool. Stateless and safe for concurrent
// use.
type ToolFeedback struct{}

// NewToolFeedback returns the tool.
func NewToolFeedback() *ToolFeedback { return &ToolFeedback{} }

func (*ToolFeedback) Name() string                 { return "tool_feedback" }
func (*ToolFeedback) InputSchema() json.RawMessage { return toolFeedbackSchema }
func (*ToolFeedback) Description() string {
	return "Tell plumb's maintainers, in a sentence, what a plumb answer lacked (missing) or carried needlessly (noise), " +
		"e.g. after context_for_task. Kept with plumb's local call record; `plumb stats --context` lists the notes. " +
		"Read-only; no network. Never include secrets or source."
}

type toolFeedbackArgs struct {
	Tool    string `json:"tool"`
	Missing string `json:"missing"`
	Noise   string `json:"noise"`
	Note    string `json:"note"`
}

func (t *ToolFeedback) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var a toolFeedbackArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", badArgument(fmt.Errorf("tool_feedback: invalid arguments: %w", err))
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	return "Noted for " + a.Tool + ". It stays with plumb's local call record, and `plumb stats --context` lists it.", nil
}

func (a toolFeedbackArgs) validate() error {
	if strings.TrimSpace(a.Tool) == "" {
		return badArgument(errors.New("tool_feedback: tool is required: name the plumb tool the note is about"))
	}
	if strings.TrimSpace(a.Missing+a.Noise+a.Note) == "" {
		return badArgument(errors.New("tool_feedback: say what was missing, what was noise, or add a note"))
	}
	for _, f := range [][2]string{{"tool", a.Tool}, {"missing", a.Missing}, {"noise", a.Noise}, {"note", a.Note}} {
		if len(f[1]) > toolFeedbackMaxBytes {
			return badArgument(fmt.Errorf("tool_feedback: %s is %d bytes, over the %d-byte limit; keep it to a sentence or two",
				f[0], len(f[1]), toolFeedbackMaxBytes))
		}
	}
	return nil
}
