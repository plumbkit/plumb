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

var copyFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "from": {
      "type": "string",
      "description": "Absolute path or file:// URI of the source file."
    },
    "to": {
      "type": "string",
      "description": "Absolute path or file:// URI of the destination. Parent directories are created automatically."
    },
    "overwrite": {
      "type": "boolean",
      "description": "Allow overwriting an existing destination file. Default false."
    },
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow copying a file that has uncommitted changes. Default false."
    }
  },
  "required": ["from", "to"],
  "additionalProperties": false
}`)

// CopyFile duplicates a file to a new path, preserving its permissions.
// Cross-device copying is supported (the destination is written via safeWrite,
// which uses a temp-file+rename approach with an EXDEV fallback; no os.Rename
// dependency on the source). The LSP server is notified with FileCreated for
// the destination so symbol indexes update immediately.
//
// To move or rename a file, use rename_file instead.
//
// Concurrency: Execute is safe for concurrent use. Both source and destination
// paths are locked to serialise with concurrent write_file/edit_file.
type CopyFile struct{ deps WriteDeps }

func NewCopyFile(deps WriteDeps) *CopyFile { return &CopyFile{deps: deps} }

func (*CopyFile) Name() string                 { return "copy_file" }
func (*CopyFile) InputSchema() json.RawMessage { return copyFileSchema }
func (*CopyFile) Description() string {
	return "Copy a file to a new path, preserving file permissions. " +
		"Parent directories of `to` are created if missing. " +
		"Refuses to overwrite an existing destination unless overwrite=true. " +
		"Cross-device copies are supported. " +
		"Notifies the LSP server with FileCreated so diagnostics update immediately. " +
		"To move or rename a file, use rename_file instead."
}

type copyFileArgs struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
	DirtyOk   bool   `json:"dirty_ok"`
}

func (t *CopyFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("copy_file", t.deps.Limiter)
	}
	a, err := parseCopyFileArgs(raw)
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

	data, perm, err := copyFilePreconditions(ctx, t.deps.Writes, from, to, a)
	if err != nil {
		return "", err
	}
	if _, err := safeWrite(to, data, perm); err != nil {
		return "", fmt.Errorf("copy_file: writing destination: %w", err)
	}
	t.copyFilePostWrite(ctx, to)
	return fmt.Sprintf("copied %s → %s (%d bytes)", from, to, len(data)), nil
}

func parseCopyFileArgs(raw json.RawMessage) (copyFileArgs, error) {
	var a copyFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("copy_file: invalid arguments: %w", err)
	}
	if a.From == "" || a.To == "" {
		return a, fmt.Errorf("copy_file: both `from` and `to` are required")
	}
	if strings.TrimPrefix(a.From, "file://") == strings.TrimPrefix(a.To, "file://") {
		return a, fmt.Errorf("copy_file: from and to are the same path")
	}
	return a, nil
}

// copyFilePreconditions validates the source, checks the dirty state, checks
// for destination conflicts, creates parent directories, and reads the source
// content. Returns the content and source permissions on success.
func copyFilePreconditions(ctx context.Context, writes *WriteTracker, from, to string, a copyFileArgs) ([]byte, os.FileMode, error) {
	info, err := os.Stat(from)
	if err != nil {
		return nil, 0, fmt.Errorf("copy_file: source: %w", err)
	}
	if info.IsDir() {
		return nil, 0, fmt.Errorf("copy_file: %q is a directory — refusing to copy recursively", from)
	}
	if !a.DirtyOk && dirtyBlocksMove(ctx, writes, from) {
		return nil, 0, fmt.Errorf("copy_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", from)
	}
	if !a.Overwrite {
		if _, err := os.Stat(to); err == nil {
			return nil, 0, fmt.Errorf("copy_file: destination %q exists (pass overwrite=true to replace)", to)
		}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return nil, 0, fmt.Errorf("copy_file: creating parent dirs: %w", err)
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return nil, 0, fmt.Errorf("copy_file: reading source: %w", err)
	}
	return data, info.Mode().Perm(), nil
}

func (t *CopyFile) copyFilePostWrite(ctx context.Context, to string) {
	if err := notifyLSP(ctx, t.deps.Client, to, protocol.FileCreated); err != nil {
		slog.Warn("copy_file: LSP create-notify failed", "path", to, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+to)
	t.deps.notifyTopology(to)
	t.deps.Writes.Record(to)
}
