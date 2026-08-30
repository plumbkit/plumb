package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/paths"
)

// rootsListProbeTimeout bounds the roots/list round-trip. The request has no
// timeout of its own and would otherwise inherit the caller's whole context —
// for a tool call, its entire budget. A client that never answers (minimal or
// half-implemented MCP clients do exist) must degrade to the documented
// defer-to-OnBeforeTool path, not stall the call; every roots/list caller
// goes through rootsFromClient, so the bound lives here. A var, not a const:
// tests shorten it to prove the bound itself, not just the behaviour around
// it — TestRootsFromClient_BoundReturnsUnderCallerDeadline fails if the
// WithTimeout below is deleted.
var rootsListProbeTimeout = 5 * time.Second

// rootFromRoots calls roots/list on the MCP client and returns the first root
// URI, or "" if the client does not support roots/list or returns no roots.
// logger scopes the log lines to the calling connection (session_id); nil falls
// back to the process default.
func rootFromRoots(ctx context.Context, request mcp.RequestFn, logger *slog.Logger) string {
	roots := rootsFromClient(ctx, request, logger)
	if len(roots) == 0 {
		return ""
	}
	loggerOr(logger).Info("workspace root from MCP client", "rootURI", roots[0])
	return roots[0]
}

// loggerOr falls back to the process default so a nil logger (tests, static
// callers) still logs instead of panicking.
func loggerOr(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// rootsFromClient returns ALL root URIs the client reports via roots/list, in
// order, or nil when it reports none or does not support roots/list. onRootsChanged
// needs the full list — not just Roots[0] — so it can tell a mere reorder (the
// pinned root is still present) from a genuine workspace removal.
func rootsFromClient(ctx context.Context, request mcp.RequestFn, logger *slog.Logger) []string {
	logger = loggerOr(logger)
	probeCtx, cancel := context.WithTimeout(ctx, rootsListProbeTimeout)
	defer cancel()
	raw, err := request(probeCtx, "roots/list", nil)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// A slow-but-alive client lands here, not in the "not supported"
			// line below: the probe had a deadline and the client outran it.
			logger.Info("roots/list probe timed out — client never answered; deferring to OnBeforeTool", "bound", rootsListProbeTimeout)
		} else {
			logger.Info("roots/list not supported by client — deferring to OnBeforeTool", "err", err)
		}
		return nil
	}

	var resp struct {
		Roots []struct {
			URI string `json:"uri"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		logger.Warn("parsing roots/list response", "err", err)
		return nil
	}
	if len(resp.Roots) == 0 {
		logger.Info("roots/list returned no roots — deferring to OnBeforeTool")
		return nil
	}

	out := make([]string, 0, len(resp.Roots))
	for _, r := range resp.Roots {
		if r.URI != "" {
			out = append(out, r.URI)
		}
	}
	return out
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

// workspaceArgPresent reports whether the tool arguments carry a non-empty
// `workspace` field — the deliberate, user-declared pin (session_start), as
// opposed to an incidental file_path/path/uri that merely happens to sit inside
// a project. Only a workspace-arg pin (or a client-reported root) is persisted
// as the sticky target across reconnects; an incidental seed is not.
func workspaceArgPresent(args json.RawMessage) bool {
	var a struct {
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(args, &a) != nil {
		return false
	}
	return a.Workspace != ""
}

// seedIsWorkspaceArg reports whether seed came from the call's `workspace`
// argument, rather than merely alongside one.
//
// workspaceArgPresent answers a different question — "may this pin be sticky?"
// — and is the wrong test for "did the caller declare THIS directory to be the
// workspace?". seedPathFromArgs prefers uri/file_path/path/root OVER workspace,
// and several tools take both: relevant_memories and write_memory each accept a
// path AND a workspace. So the two can name different directories, and treating
// presence as a declaration let an incidental path be laundered into a
// deliberate pin of a directory the caller never named — which matters most for
// $HOME, where the deliberate-pin exemption is the only way in.
//
// Compared after URI decoding, since a seed may arrive as file:// while the
// workspace argument is a plain path or vice versa.
func seedIsWorkspaceArg(args json.RawMessage, seed string) bool {
	var a struct {
		Workspace string `json:"workspace"`
	}
	if seed == "" || json.Unmarshal(args, &a) != nil || a.Workspace == "" {
		return false
	}
	return paths.URIToPath(a.Workspace) == paths.URIToPath(seed)
}

// seedPathFromArgs extracts a single filesystem path from a tool call's raw
// JSON arguments. Probes the argument shapes plumb's tools use:
//
//	{"uri": "file:///..."}                      — LSP tools
//	{"file_path": "/..."}                       — file-content tools (read/write/edit/delete)
//	{"path": "/..."}                            — search/dir tools (find_files, search_in_files, …)
//	{"root": "/..."}                            — the retired list_files spelling, still reached
//	                                              via the PARAMETER alias on a direct find_files
//	                                              call (the tool-name alias renames root→path
//	                                              before this hook ever runs)
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
