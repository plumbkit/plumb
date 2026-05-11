package mcp

import (
	"context"
	"encoding/json"
)

// PromptArgument describes one named parameter a prompt accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptMessage is one turn in a prompt's messages array.
type PromptMessage struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content PromptContent  `json:"content"`
}

// PromptContent holds the text (or other content) of a prompt message.
type PromptContent struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// Prompt is a named workflow surfaced to MCP clients (e.g. as a button in
// Claude Desktop). Clients call prompts/get to expand the prompt into a
// ready-to-send messages array.
type Prompt interface {
	// Name is the unique identifier used in prompts/list and prompts/get.
	Name() string
	// Description is shown to the user/client as a one-line summary.
	Description() string
	// Arguments declares the parameters this prompt accepts.
	Arguments() []PromptArgument
	// Expand returns the messages array for the given arguments.
	// args may be nil when the prompt has no required arguments.
	Expand(ctx context.Context, args map[string]string) ([]PromptMessage, error)
}

// RegisterPrompt adds a prompt to the server. Must be called before Serve.
func (s *Server) RegisterPrompt(p Prompt) {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	if _, exists := s.prompts[p.Name()]; !exists {
		s.promptOrder = append(s.promptOrder, p.Name())
	}
	s.prompts[p.Name()] = p
}

func (s *Server) handlePromptsList(req mcpRequest) mcpResponse {
	s.promptMu.RLock()
	defer s.promptMu.RUnlock()

	type promptDef struct {
		Name        string           `json:"name"`
		Description string           `json:"description,omitempty"`
		Arguments   []PromptArgument `json:"arguments,omitempty"`
	}
	defs := make([]promptDef, 0, len(s.promptOrder))
	for _, name := range s.promptOrder {
		p := s.prompts[name]
		defs = append(defs, promptDef{
			Name:        p.Name(),
			Description: p.Description(),
			Arguments:   p.Arguments(),
		})
	}
	return okResp(req.ID, map[string]any{"prompts": defs})
}

func (s *Server) handlePromptsGet(ctx context.Context, req mcpRequest) mcpResponse {
	var params struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		return errResp(req.ID, codeInvalidParams, "name required")
	}

	s.promptMu.RLock()
	p, ok := s.prompts[params.Name]
	s.promptMu.RUnlock()
	if !ok {
		return errResp(req.ID, codeMethodNotFound, "unknown prompt: "+params.Name)
	}

	msgs, err := p.Expand(ctx, params.Arguments)
	if err != nil {
		return errResp(req.ID, -32000, "prompt expand failed: "+err.Error())
	}

	type result struct {
		Description string          `json:"description,omitempty"`
		Messages    []PromptMessage `json:"messages"`
	}
	return okResp(req.ID, result{
		Description: p.Description(),
		Messages:    msgs,
	})
}
