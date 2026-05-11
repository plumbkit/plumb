package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ResourceDescriptor describes one resource returned by resources/list.
// MimeType is optional but recommended — Claude Desktop uses it to render
// content in a viewer (e.g. "text/markdown" enables markdown formatting).
type ResourceDescriptor struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceContent is one body returned by resources/read. Either Text or
// Blob is set, never both. Blob (if used) is base64-encoded by the caller.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ResourceProvider lists and reads resources exposed to MCP clients.
//
// MCP "resources" are a separate primitive from "tools": clients (notably
// Claude Desktop) surface them in a browsable panel rather than requiring
// the LLM to call a tool to fetch them. Use this for read-only artifacts
// like saved memories, generated reports, or project context files.
type ResourceProvider interface {
	List(ctx context.Context) ([]ResourceDescriptor, error)
	Read(ctx context.Context, uri string) (*ResourceContent, error)
}

func (s *Server) handleResourcesList(ctx context.Context, req mcpRequest) mcpResponse {
	if s.Resources == nil {
		return errResp(req.ID, codeMethodNotFound, "resources not supported")
	}
	descs, err := s.Resources.List(ctx)
	if err != nil {
		return errResp(req.ID, -32000, "resources/list failed: "+err.Error())
	}
	if descs == nil {
		descs = []ResourceDescriptor{}
	}
	return okResp(req.ID, map[string]any{"resources": descs})
}

func (s *Server) handleResourcesRead(ctx context.Context, req mcpRequest) mcpResponse {
	if s.Resources == nil {
		return errResp(req.ID, codeMethodNotFound, "resources not supported")
	}
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.URI == "" {
		return errResp(req.ID, codeInvalidParams, "uri required")
	}
	content, err := s.Resources.Read(ctx, p.URI)
	if err != nil {
		return errResp(req.ID, -32000, fmt.Sprintf("resources/read %q: %v", p.URI, err))
	}
	return okResp(req.ID, map[string]any{"contents": []*ResourceContent{content}})
}
