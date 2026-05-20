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
    },
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow moving a file that has uncommitted changes in its git repository. Default false — the move is refused if the source file is dirty. Pass true to proceed anyway."
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
	return "Move (rename) a file — this is the primary tool for moving files. " +
		"Parent directories of `to` are created if missing. " +
		"Refuses to overwrite an existing destination unless overwrite=true. The LSP server " +
		"is notified with FileDeleted (source) and FileCreated (destination) so symbol " +
		"indexes and diagnostics update immediately. " +
		"To duplicate a file without removing the source, use copy_file instead. " +
		"For LSP-semantic identifier renames across files, use rename_symbol instead."
}

type renameFileArgs struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
	DirtyOk   bool   `json:"dirty_ok"`
}

func (t *RenameFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("rename_file", t.deps.Limiter)
	}
	a, err := parseRenameFileArgs(raw)
	if err != nil {
		return "", err
	}
	from := strings.TrimPrefix(a.From, "file://")
	to := strings.TrimPrefix(a.To, "file://")

	// Lock both paths in lexical order to prevent deadlocks.
	first, second := from, to
	if first > second {
		first, second = second, first
	}
	unlock1 := lockPath(first)
	defer unlock1()
	unlock2 := lockPath(second)
	defer unlock2()

	if err := renameFilePreconditions(ctx, from, to, a); err != nil {
		return "", err
	}
	if err := os.Rename(from, to); err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}
	t.renameFilePostRename(ctx, from, to)
	return fmt.Sprintf("renamed %s → %s", from, to), nil
}

func parseRenameFileArgs(raw json.RawMessage) (renameFileArgs, error) {
	var a renameFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("rename_file: invalid arguments: %w", err)
	}
	if a.From == "" || a.To == "" {
		return a, fmt.Errorf("rename_file: both `from` and `to` are required")
	}
	if strings.TrimPrefix(a.From, "file://") == strings.TrimPrefix(a.To, "file://") {
		return a, fmt.Errorf("rename_file: from and to are the same path")
	}
	return a, nil
}

func renameFilePreconditions(ctx context.Context, from, to string, a renameFileArgs) error {
	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("rename_file: source: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("rename_file: %q is a directory — refusing to move recursively", from)
	}
	if !a.DirtyOk && pathIsDirty(ctx, from) {
		return fmt.Errorf("rename_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", from)
	}
	if !a.Overwrite {
		if _, err := os.Stat(to); err == nil {
			return fmt.Errorf("rename_file: destination %q exists (pass overwrite=true to replace)", to)
		}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("rename_file: creating parent dirs: %w", err)
	}
	return nil
}

func (t *RenameFile) renameFilePostRename(ctx context.Context, from, to string) {
	if err := notifyLSP(ctx, t.deps.Client, from, protocol.FileDeleted); err != nil {
		slog.Warn("rename_file: LSP delete-notify failed", "path", from, "err", err)
	}
	if err := notifyLSP(ctx, t.deps.Client, to, protocol.FileCreated); err != nil {
		slog.Warn("rename_file: LSP create-notify failed", "path", to, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+from)
	invalidateCache(t.deps.Cache, "file://"+to)
}
