package cli

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/paths"
)

// rootFromRoots calls roots/list on the MCP client and returns the first root
// URI, or "" if the client does not support roots/list or returns no roots.
func rootFromRoots(ctx context.Context, request mcp.RequestFn) string {
	raw, err := request(ctx, "roots/list", nil)
	if err != nil {
		slog.Info("roots/list not supported by client — deferring to OnBeforeTool", "err", err)
		return ""
	}

	var resp struct {
		Roots []struct {
			URI string `json:"uri"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		slog.Warn("parsing roots/list response", "err", err)
		return ""
	}
	if len(resp.Roots) == 0 {
		slog.Info("roots/list returned no roots — deferring to OnBeforeTool")
		return ""
	}

	root := resp.Roots[0].URI
	slog.Info("workspace root from MCP client", "rootURI", root)
	return root
}

// workspaceFromArgs returns the resolved workspace root for a tool call's raw
// JSON arguments. Returns "" if no path-bearing field is present or the path
// doesn't sit under a discoverable project root.
func workspaceFromArgs(pool *workspacePool, args json.RawMessage) string {
	seed := seedPathFromArgs(args)
	if seed == "" {
		return ""
	}
	// If seed is already a directory, use it directly — filepath.Dir would
	// strip the last component and miss the project root marker.
	startDir := seed
	if info, err := os.Stat(seed); err != nil || !info.IsDir() {
		startDir = filepath.Dir(seed)
	}
	root, _, err := pool.Detect(startDir)
	if err != nil {
		return ""
	}
	return root
}

// seedPathFromArgs extracts a single filesystem path from a tool call's raw
// JSON arguments. Probes the argument shapes plumb's tools use:
//
//	{"uri": "file:///..."}                      — LSP tools
//	{"file_path": "/..."}                       — file-content tools (read/write/edit/delete)
//	{"path": "/..."}                            — search/dir tools (list_directory, find_files, …)
//	{"root": "/..."}                            — list_files
//	{"workspace": "/..."}                       — session_start
//	{"paths": ["/...", ...]}                    — read_multiple_files
//	{"operations": [{"path": "/..."}, ...]}     — transaction_apply
//
// Returns "" if no shape matches. Any leading file:// is stripped so the
// caller gets a plain filesystem path.
func seedPathFromArgs(args json.RawMessage) string {
	var a struct {
		URI        string   `json:"uri"`
		FilePath   string   `json:"file_path"`
		Path       string   `json:"path"`
		Root       string   `json:"root"`
		Workspace  string   `json:"workspace"`
		Paths      []string `json:"paths"`
		Operations []struct {
			FilePath string `json:"file_path"`
			Path     string `json:"path"`
		} `json:"operations"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	switch {
	case a.URI != "":
		return paths.URIToPath(a.URI)
	case a.FilePath != "":
		return a.FilePath
	case a.Path != "":
		return a.Path
	case a.Root != "":
		return a.Root
	case a.Workspace != "":
		return a.Workspace
	case len(a.Paths) > 0:
		return a.Paths[0]
	case len(a.Operations) > 0:
		if a.Operations[0].FilePath != "" {
			return a.Operations[0].FilePath
		}
		return a.Operations[0].Path
	}
	return ""
}
