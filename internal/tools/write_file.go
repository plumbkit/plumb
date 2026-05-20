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
    },
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow writing a file that has uncommitted changes in its git repository. Default false — the write is refused if the target file is dirty. Pass true to overwrite anyway."
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

func (*WriteFile) Name() string                 { return "write_file" }
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
	DirtyOk    bool   `json:"dirty_ok"`
}

func (t *WriteFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("write_file", t.deps.Limiter)
	}
	a, err := parseWriteFileArgs(raw)
	if err != nil {
		return "", err
	}

	path := strings.TrimPrefix(a.Path, "file://")

	unlock := lockPath(path)
	defer unlock()

	if err := t.writeFilePreconditions(ctx, path, a); err != nil {
		return "", err
	}

	_, statErr := os.Stat(path)
	isNew := os.IsNotExist(statErr)
	uri := "file://" + path

	oldContent, preDiags := t.writeFilePreState(uri, path, isNew)

	if _, err := safeWrite(path, []byte(a.Content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}

	t.writeFilePostWrite(ctx, path, uri, isNew)
	return t.formatWriteFileResult(path, a.Content, oldContent, isNew, uri, preDiags), nil
}

func parseWriteFileArgs(raw json.RawMessage) (writeFileArgs, error) {
	var a writeFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("write_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return a, fmt.Errorf("write_file: path is required")
	}
	// content is schema-required; the MCP layer rejects missing keys.
	// An explicit empty string is allowed (e.g. truncating a file).
	return a, nil
}

func (t *WriteFile) writeFilePreconditions(ctx context.Context, path string, a writeFileArgs) error {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return fmt.Errorf("write_file: %q is a directory", path)
	}
	if !a.DirtyOk && pathIsDirty(ctx, path) {
		return fmt.Errorf("write_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to overwrite", path)
	}
	createDirs := a.CreateDirs == nil || *a.CreateDirs
	if !createDirs {
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			return fmt.Errorf("write_file: parent directory does not exist (set create_dirs=true to create it): %w", err)
		}
	}
	return nil
}

// writeFilePreState captures the pre-write state needed for diff output and
// diagnostic comparison. Returns empty strings/nil when the deps are not wired.
func (t *WriteFile) writeFilePreState(uri, path string, isNew bool) (oldContent string, preDiags []protocol.Diagnostic) {
	if !isNew && t.deps.showWriteDiff() {
		if b, err := os.ReadFile(path); err == nil && len(b) <= 200*1024 {
			oldContent = string(b)
		}
	}
	if t.deps.Diag != nil {
		preDiags = t.deps.Diag.Diagnostics(uri)
	}
	return oldContent, preDiags
}

func (t *WriteFile) writeFilePostWrite(ctx context.Context, path, uri string, isNew bool) {
	changeType := protocol.FileChanged
	if isNew {
		changeType = protocol.FileCreated
	}
	if err := notifyLSP(ctx, t.deps.Client, path, changeType); err != nil {
		slog.Warn("write_file: LSP notification failed", "path", path, "err", err)
	}
	if t.deps.PostWriteNotifyFn != nil {
		if err := t.deps.PostWriteNotifyFn(ctx, path); err != nil {
			slog.Warn("write_file: post-write adapter notification failed", "path", path, "err", err)
		}
	}
	invalidateCache(t.deps.Cache, uri)
}

func (t *WriteFile) formatWriteFileResult(path, newContent, oldContent string, isNew bool, uri string, preDiags []protocol.Diagnostic) string {
	verb := "updated"
	if isNew {
		verb = "created"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s (%d bytes)", verb, path, len(newContent))
	if t.deps.showWriteDiff() {
		if isNew {
			sb.WriteString("\nnew file")
		} else if d := unifiedDiff(path, oldContent, newContent); d != "" {
			sb.WriteString("\n")
			sb.WriteString(d)
		}
	}
	if t.deps.Diag != nil {
		fresh := awaitDiagnosticsRefresh(t.deps.Diag, uri, preDiags, t.deps.postWriteDiagWindow())
		sb.WriteString(formatPostWriteDiagnostics(fresh))
	}
	return sb.String()
}
