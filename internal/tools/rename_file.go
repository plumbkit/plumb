package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var renameFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "from": {
      "type": "string",
      "description": "Absolute path or file:// URI of the source file."
    },
    "to": {
      "type": "string",
      "description": "Absolute path or file:// URI of the destination file. Parent directories are created automatically."
    },
    "overwrite": {
      "type": "boolean",
      "description": "Allow overwriting an existing destination file. Default false."
    }
  },
  "required": ["from", "to"]
}`)

// RenameFile moves/renames a single file. Notifies the LSP server with both
// FileDeleted (source) and FileCreated (destination) so symbol indexes
// transfer cleanly.
//
// Notably distinct from rename_symbol (LSP-semantic rename of an identifier).
// This is a filesystem-level operation.
//
// Concurrency: Execute is safe for concurrent use. Both source and destination
// paths are locked to serialise with any concurrent write_file/edit_file.
type RenameFile struct{ deps WriteDeps }

func NewRenameFile(deps WriteDeps) *RenameFile { return &RenameFile{deps: deps} }

func (*RenameFile) Name() string                 { return "rename_file" }
func (*RenameFile) InputSchema() json.RawMessage { return renameFileSchema }
func (*RenameFile) Description() string {
	return "Move or rename a file. Parent directories of `to` are created if missing. " +
		"Refuses to overwrite an existing destination unless overwrite=true. The LSP server " +
		"is notified with FileDeleted (source) and FileCreated (destination) so symbol " +
		"indexes and diagnostics update immediately. For LSP-semantic identifier renames " +
		"across files, use rename_symbol instead."
}

type renameFileArgs struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
}

func (t *RenameFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("rename_file", t.deps.Limiter)
	}
	var a renameFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("rename_file: invalid arguments: %w", err)
	}
	if a.From == "" || a.To == "" {
		return "", fmt.Errorf("rename_file: both `from` and `to` are required")
	}
	from := strings.TrimPrefix(a.From, "file://")
	to := strings.TrimPrefix(a.To, "file://")
	if from == to {
		return "", fmt.Errorf("rename_file: from and to are the same path")
	}

	// Lock both paths. Order by lexical sort to avoid deadlocks between
	// two concurrent rename_file calls that swap two files.
	first, second := from, to
	if first > second {
		first, second = second, first
	}
	unlock1 := lockPath(first)
	defer unlock1()
	unlock2 := lockPath(second)
	defer unlock2()

	info, err := os.Stat(from)
	if err != nil {
		return "", fmt.Errorf("rename_file: source: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("rename_file: %q is a directory — refusing to move recursively", from)
	}

	if !a.Overwrite {
		if _, err := os.Stat(to); err == nil {
			return "", fmt.Errorf("rename_file: destination %q exists (pass overwrite=true to replace)", to)
		}
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", fmt.Errorf("rename_file: creating parent dirs: %w", err)
	}

	if err := os.Rename(from, to); err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}

	if err := notifyLSP(ctx, t.deps.Client, from, protocol.FileDeleted); err != nil {
		slog.Warn("rename_file: LSP delete-notify failed", "path", from, "err", err)
	}
	if err := notifyLSP(ctx, t.deps.Client, to, protocol.FileCreated); err != nil {
		slog.Warn("rename_file: LSP create-notify failed", "path", to, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+from)
	invalidateCache(t.deps.Cache, "file://"+to)

	return fmt.Sprintf("renamed %s → %s", from, to), nil
}
