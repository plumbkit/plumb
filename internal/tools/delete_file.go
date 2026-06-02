package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var deleteFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "Absolute path or file:// URI of the file or empty directory to delete."
    },
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow deleting a file that has uncommitted changes in its git repository. Default false — deletion is refused if the file is dirty. Pass true to proceed anyway."
    },
    "allow_dir": {
      "type": "boolean",
      "description": "Allow deleting an empty directory. Default false — deletion is refused for any directory. The directory must be empty; non-empty directories are rejected even with allow_dir: true."
    }
  },
  "required": ["file_path"],
  "additionalProperties": false
}`)

// DeleteFile removes a single file (not a directory) and notifies the LSP
// server via workspace/didChangeWatchedFiles with FileDeleted so symbol
// indexes drop the file's contents immediately.
//
// Concurrency: Execute is safe for concurrent use.
type DeleteFile struct{ deps WriteDeps }

func NewDeleteFile(deps WriteDeps) *DeleteFile { return &DeleteFile{deps: deps} }

func (*DeleteFile) Name() string                 { return "delete_file" }
func (*DeleteFile) InputSchema() json.RawMessage { return deleteFileSchema }
func (*DeleteFile) Description() string {
	return "Delete a single file or empty directory. Refuses to delete directories unless allow_dir: true is set — " +
		"and even then only an empty directory is accepted (non-empty directories are always rejected). " +
		"For a directory tree, delete files individually with repeated delete_file calls, then remove each " +
		"now-empty directory with allow_dir: true. The LSP server is notified with FileDeleted so symbol " +
		"indexes and diagnostics update immediately. Per-path locking serialises against any concurrent " +
		"write_file/edit_file targeting the same path."
}

type deleteFileArgs struct {
	Path     string `json:"file_path"`
	DirtyOk  bool   `json:"dirty_ok"`
	AllowDir bool   `json:"allow_dir"`
}

func (t *DeleteFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("delete_file", t.deps.Limiter)
	}
	var a deleteFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("delete_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("delete_file: file_path is required")
	}
	path := strings.TrimPrefix(a.Path, "file://")
	if err := t.deps.checkBoundary(path); err != nil {
		return "", fmt.Errorf("delete_file: %w", err)
	}

	unlock := lockPath(path)
	defer unlock()

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("delete_file: %w", err)
	}
	if info.IsDir() {
		if !a.AllowDir {
			return "", fmt.Errorf("delete_file: %q is a directory — pass allow_dir: true to delete an empty directory", path)
		}
		if err := os.Remove(path); err != nil {
			return "", fmt.Errorf("delete_file: %w (directory must be empty)", err)
		}
		t.deps.notifyTopology(path)
		return fmt.Sprintf("deleted directory %s", path), nil
	}

	if !a.DirtyOk && dirtyBlocksWrite(ctx, t.deps.Writes, path) {
		return "", fmt.Errorf("delete_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", path)
	}

	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("delete_file: %w", err)
	}

	if err := notifyLSP(ctx, t.deps.Client, path, protocol.FileDeleted); err != nil {
		slog.Warn("delete_file: LSP notification failed", "path", path, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+path)
	// processUpsert detects the missing file and routes to processDelete automatically.
	t.deps.notifyTopology(path)

	return fmt.Sprintf("deleted %s", path), nil
}
