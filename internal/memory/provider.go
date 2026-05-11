package memory

import (
	"context"
	"fmt"
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
	mems, err := List(ws)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ResourceDescriptor, 0, len(mems))
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
