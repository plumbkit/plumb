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

var writeFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Absolute path or file:// URI of the file to write."
    },
    "content": {
      "type": "string",
      "description": "Full content to write to the file."
    },
    "create_dirs": {
      "type": "boolean",
      "description": "Create parent directories if they do not exist. Default true."
    }
  },
  "required": ["path", "content"]
}`)

// WriteFile creates or overwrites a file atomically.
//
// Safety model:
//   - Content is staged in a temp file in os.TempDir() (no project-tree noise),
//     then renamed into place. os.Rename is atomic on POSIX — the target is
//     never partially written. If the temp dir and target are on different
//     filesystems (EXDEV), a .plumb.tmp sibling is used automatically.
//   - Existing file permissions are preserved. New files get 0644.
//   - After a successful write the LSP server receives didOpen/didChange/
//     didClose so its in-memory view stays consistent immediately.
//
// Concurrency: Execute is safe for concurrent use.
type WriteFile struct {
	deps WriteDeps
}

func NewWriteFile(deps WriteDeps) *WriteFile { return &WriteFile{deps: deps} }

func (*WriteFile) Name() string               { return "write_file" }
func (*WriteFile) InputSchema() json.RawMessage { return writeFileSchema }
func (*WriteFile) Description() string {
	return "Create or overwrite a file with the given content. The write is atomic: " +
		"content is staged in a system temp file then renamed into place — the " +
		"target is never partially written. Parent directories are created automatically. " +
		"After writing, the LSP server is notified (didOpen/didChange/didClose) so " +
		"diagnostics and symbol lookups reflect the new content immediately. " +
		"Use edit_file for targeted str_replace edits to an existing file."
}

type writeFileArgs struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	CreateDirs *bool  `json:"create_dirs"`
}

func (t *WriteFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("write_file", t.deps.Limiter)
	}
	var a writeFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("write_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("write_file: path is required")
	}
	// content is a schema-required field; the MCP layer rejects missing keys.
	// An explicit empty string is allowed (e.g. truncating a file).

	path := strings.TrimPrefix(a.Path, "file://")

	// Per-path lock: serialise with any concurrent edit_file / write_file
	// targeting the same on-disk path.
	unlock := lockPath(path)
	defer unlock()

	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", fmt.Errorf("write_file: %q is a directory", path)
	}

	createDirs := true
	if a.CreateDirs != nil {
		createDirs = *a.CreateDirs
	}
	if !createDirs {
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			return "", fmt.Errorf("write_file: parent directory does not exist (set create_dirs=true to create it): %w", err)
		}
	}

	_, statErr := os.Stat(path)
	isNew := os.IsNotExist(statErr)

	uri := "file://" + path
	var preDiags []protocol.Diagnostic
	if t.deps.Diag != nil {
		preDiags = t.deps.Diag.Diagnostics(uri)
	}

	if _, err := safeWrite(path, []byte(a.Content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}

	changeType := protocol.FileChanged
	if isNew {
		changeType = protocol.FileCreated
	}
	if err := notifyLSP(ctx, t.deps.Client, path, changeType); err != nil {
		slog.Warn("write_file: LSP notification failed", "path", path, "err", err)
	}
	invalidateCache(t.deps.Cache, uri)

	verb := "updated"
	if isNew {
		verb = "created"
	}
	out := fmt.Sprintf("%s %s (%d bytes)", verb, path, len(a.Content))
	if t.deps.Diag != nil {
		fresh := awaitDiagnosticsRefresh(t.deps.Diag, uri, preDiags, t.deps.PostWriteDiagWindow)
		out += formatPostWriteDiagnostics(fresh)
	}
	return out, nil
}

