package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

var deleteFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the file or empty directory to delete."
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
		"write_file/edit_file targeting the same path. The response reports the line and byte count " +
		"removed (bytes only for a binary or oversized file)."
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
	path := t.deps.resolvePath(a.Path)
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

	if !a.DirtyOk && dirtyBlocksWrite(ctx, t.deps, path) {
		return "", fmt.Errorf("delete_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", path)
	}

	// Summarise what is about to be removed (line + byte count) before deleting,
	// so the agent can report the scope of the change. Best-effort: a read error
	// degrades to the byte count from Stat.
	summary := deleteSummary(path, info.Size())

	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("delete_file: %w", err)
	}

	if err := notifyLSP(ctx, t.deps.Client, path, protocol.FileDeleted); err != nil {
		slog.Warn("delete_file: LSP notification failed", "path", path, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+path)
	// processUpsert detects the missing file and routes to processDelete automatically.
	t.deps.notifyTopology(path)

	return fmt.Sprintf("deleted %s — %s", path, summary), nil
}

// deleteSummary describes the content removed by a delete: a line + byte count
// for a readable text file, falling back to bytes only for a binary file, one
// over maxReadFileBytes, or any that can't be read. size is the Stat size, used
// for the byte count and to skip reading oversized files.
func deleteSummary(path string, size int64) string {
	if size > maxReadFileBytes {
		return fmt.Sprintf("%d bytes removed", size)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("%d bytes removed", size)
	}
	sniff := data
	if len(sniff) > binarySniffBytes {
		sniff = sniff[:binarySniffBytes]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return fmt.Sprintf("%d bytes removed (binary)", len(data))
	}
	return fmt.Sprintf("%d lines, %d bytes removed", countTextLines(data), len(data))
}

// countTextLines counts lines the way an editor would: the number of newlines,
// plus one for a final line with no trailing newline. Empty content is 0 lines.
func countTextLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}
