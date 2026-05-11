package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/golimpio/plumb/internal/mcp"
)

// ResourceProvider exposes a workspace's memories as MCP resources so they
// appear in clients (like Claude Desktop) that surface resources in a
// browsable panel. Each memory becomes one resource with a markdown mime
// type, so the client can render it natively without invoking a tool.
//
// The provider resolves the workspace lazily via a getter so a single
// instance can be constructed at connection start (before the workspace is
// known) and still produce correct results once it is.
type ResourceProvider struct {
	workspaceFn func() string
}

// NewResourceProvider creates a memory ResourceProvider. workspaceFn returns
// the current connection's primary workspace, or "" if none has been
// resolved yet (in which case List returns an empty list).
func NewResourceProvider(workspaceFn func() string) *ResourceProvider {
	return &ResourceProvider{workspaceFn: workspaceFn}
}

const uriScheme = "plumb-memory://"
const contextURI = "plumb://workspace/context"

// contextMDPath returns the path to .plumb/context.md for a workspace.
func contextMDPath(workspace string) string {
	return filepath.Join(workspace, ".plumb", "context.md")
}

// uriFor returns the MCP resource URI for a named memory.
func uriFor(name string) string { return uriScheme + name }

func nameFromURI(uri string) (string, bool) {
	if !strings.HasPrefix(uri, uriScheme) {
		return "", false
	}
	return strings.TrimPrefix(uri, uriScheme), true
}

func (p *ResourceProvider) List(_ context.Context) ([]mcp.ResourceDescriptor, error) {
	ws := p.workspaceFn()
	if ws == "" {
		return nil, nil
	}

	var out []mcp.ResourceDescriptor

	// Expose context.md as the first resource if it exists.
	if _, err := os.Stat(contextMDPath(ws)); err == nil {
		out = append(out, mcp.ResourceDescriptor{
			URI:         contextURI,
			Name:        "Project context",
			Description: "Project overview, architecture, conventions, and gotchas from .plumb/context.md",
			MimeType:    "text/markdown",
		})
	}

	mems, err := List(ws)
	if err != nil {
		return nil, err
	}
	for _, m := range mems {
		desc := m.Description
		if desc == "" {
			desc = fmt.Sprintf("Memory in %s/.plumb/memories/", filepath.Base(ws))
		}
		out = append(out, mcp.ResourceDescriptor{
			URI:         uriFor(m.Name),
			Name:        m.Name,
			Description: desc,
			MimeType:    "text/markdown",
		})
	}
	return out, nil
}

func (p *ResourceProvider) Read(_ context.Context, uri string) (*mcp.ResourceContent, error) {
	// context.md resource.
	if uri == contextURI {
		ws := p.workspaceFn()
		if ws == "" {
			return nil, fmt.Errorf("no workspace resolved; cannot read context")
		}
		data, err := os.ReadFile(contextMDPath(ws))
		if err != nil {
			return nil, fmt.Errorf("reading context.md: %w", err)
		}
		return &mcp.ResourceContent{
			URI:      uri,
			MimeType: "text/markdown",
			Text:     string(data),
		}, nil
	}

	// Memory resources.
	name, ok := nameFromURI(uri)
	if !ok {
		return nil, fmt.Errorf("unsupported URI scheme: %s", uri)
	}
	ws := p.workspaceFn()
	if ws == "" {
		return nil, fmt.Errorf("no workspace resolved; cannot read memory")
	}
	text, err := Read(ws, name)
	if err != nil {
		return nil, err
	}
	return &mcp.ResourceContent{
		URI:      uri,
		MimeType: "text/markdown",
		Text:     text,
	}, nil
}
