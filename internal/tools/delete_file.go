package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var deleteFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Absolute path or file:// URI of the file to delete."
    }
  },
  "required": ["path"]
}`)

// DeleteFile removes a single file (not a directory) and notifies the LSP
// server via workspace/didChangeWatchedFiles with FileDeleted so symbol
// indexes drop the file's contents immediately.
//
// Concurrency: Execute is safe for concurrent use.
type DeleteFile struct {
	client  lsp.LSPClient // may be nil; LSP notify skipped when nil
	cache   *cache.Cache  // may be nil; cache invalidation skipped when nil
	limiter *RateLimiter  // may be nil; rate limiting skipped when nil
}

func NewDeleteFile(client lsp.LSPClient, c *cache.Cache, lim *RateLimiter) *DeleteFile {
	return &DeleteFile{client: client, cache: c, limiter: lim}
}

func (*DeleteFile) Name() string                 { return "delete_file" }
func (*DeleteFile) InputSchema() json.RawMessage { return deleteFileSchema }
func (*DeleteFile) Description() string {
	return "Delete a single file. Refuses to delete directories — use shell tools " +
		"for recursive removal. The LSP server is notified with FileDeleted so " +
		"symbol indexes and diagnostics update immediately. Per-path locking " +
		"serialises against any concurrent write_file/edit_file targeting the same path."
}

type deleteFileArgs struct {
	Path string `json:"path"`
}

func (t *DeleteFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.limiter.Allow() {
		return "", rateLimitError("delete_file", t.limiter)
	}
	var a deleteFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("delete_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("delete_file: path is required")
	}
	path := strings.TrimPrefix(a.Path, "file://")

	unlock := lockPath(path)
	defer unlock()

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("delete_file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("delete_file: %q is a directory — refusing to delete recursively", path)
	}

	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("delete_file: %w", err)
	}

	if err := notifyLSP(ctx, t.client, path, protocol.FileDeleted); err != nil {
		slog.Warn("delete_file: LSP notification failed", "path", path, "err", err)
	}
	invalidateCache(t.cache, "file://"+path)

	return fmt.Sprintf("deleted %s", path), nil
}
