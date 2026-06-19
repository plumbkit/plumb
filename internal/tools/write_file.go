package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

var writeFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the file to write."
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
    },
    "overwrite_changed": {
      "type": "boolean",
      "description": "Allow overwriting a file that changed on disk since this session read it (a peer agent or human edited it after your read). Default false — the write is refused so a stale full-content overwrite cannot silently discard that change. Re-read to merge, or pass true to overwrite anyway. Only consulted when neither expected_mtime nor expected_sha is given (those guards take precedence)."
    },
    "expected_mtime": {
      "type": "string",
      "description": "Optional. RFC3339Nano mtime previously returned by read_file. If provided, the write is rejected if the file's current mtime differs — fast optimistic-concurrency check, so a full-content overwrite never silently clobbers a change made since you read it."
    },
    "expected_sha": {
      "type": "string",
      "description": "Optional. Hex-encoded SHA-256 previously returned by read_file. If provided, the write is rejected if the file's current content hash differs — stronger than expected_mtime, survives mtime aliasing."
    },
    "await_diagnostics": {
      "type": "boolean",
      "description": "When true, block up to a few seconds for the language server to finish re-analysing this file and report an authoritative post-write result — a clean fresh pass is stated explicitly. Use it for a trustworthy \"did my change compile?\" answer instead of shelling out to a build. Default false (fast adaptive window; the result may predate the write)."
    }
  },
  "required": ["file_path", "content"],
  "additionalProperties": false
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
	return "Create or overwrite a file with the given content. The write is atomic (staged in a temp file " +
		"then renamed — never partially written); parent directories are created automatically and the LSP " +
		"server is notified so diagnostics and symbols update immediately. " +
		"Pass expected_mtime or expected_sha (from a read_file header) to reject the write if the file changed " +
		"since you read it, so a full-content overwrite never silently clobbers a concurrent change. " +
		"If the call fails with a transport/connection error, the atomic temp+rename guarantees the file " +
		"is either fully written or untouched — never partially written; re-read to confirm which side of " +
		"the rename it landed on. " +
		"Use edit_file for targeted edits to an existing file."
}

type writeFileArgs struct {
	Path             string `json:"file_path"`
	Content          string `json:"content"`
	CreateDirs       *bool  `json:"create_dirs"`
	DirtyOk          bool   `json:"dirty_ok"`
	AwaitDiagnostics bool   `json:"await_diagnostics"`
	ExpectedMtime    string `json:"expected_mtime"`
	ExpectedSha      string `json:"expected_sha"`
	OverwriteChanged bool   `json:"overwrite_changed"`
}

func (t *WriteFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("write_file", t.deps.Limiter)
	}
	a, err := parseWriteFileArgs(raw)
	if err != nil {
		return "", err
	}

	path := t.deps.resolvePath(a.Path)
	if err := t.deps.checkBoundary(path); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}

	unlock := lockPath(path)
	defer unlock()

	if err := t.writeFilePreconditions(ctx, path, a); err != nil {
		return "", err
	}

	_, statErr := os.Stat(path)
	isNew := os.IsNotExist(statErr)
	uri := "file://" + path

	oldContent, undoBefore, undoOK := t.writeFileCapture(path, isNew)

	if _, err := safeWrite(path, []byte(a.Content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}

	t.writeFilePostWrite(ctx, path, uri, isNew)
	if undoOK {
		t.deps.recordUndo(path, undoBefore, a.Content, !isNew, "write_file")
	}
	result := t.formatWriteFileResult(path, a.Content, oldContent, isNew, uri, a.AwaitDiagnostics)
	t.deps.notifyTopology(path)
	return result + t.deps.reportQuality(ctx, path), nil
}

func parseWriteFileArgs(raw json.RawMessage) (writeFileArgs, error) {
	var a writeFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("write_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return a, fmt.Errorf("write_file: file_path is required")
	}
	// content is schema-required; the MCP layer rejects missing keys.
	// An explicit empty string is allowed (e.g. truncating a file).
	return a, nil
}

func (t *WriteFile) writeFilePreconditions(ctx context.Context, path string, a writeFileArgs) error {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return fmt.Errorf("write_file: %q is a directory", path)
	}
	if !a.DirtyOk && dirtyBlocksWrite(ctx, t.deps.Writes, path) {
		return fmt.Errorf("write_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to overwrite", path)
	}
	if err := verifyExpectedVersion("write_file", path, a.ExpectedMtime, a.ExpectedSha); err != nil {
		return err
	}
	// Automatic session-aware guard: if the caller gave no explicit version guard
	// but this session read the file and it has since changed on disk, a full
	// overwrite would silently discard that change. Refuse unless overridden.
	if a.ExpectedMtime == "" && a.ExpectedSha == "" && !a.OverwriteChanged &&
		changedSinceSessionRead(t.deps.Reads, path) {
		return fmt.Errorf("write_file: %q changed on disk since you read it this session — "+
			"a peer agent or process edited it after your read, and a full overwrite would discard that change. "+
			"Re-read to merge, or pass overwrite_changed: true to overwrite anyway", path)
	}
	createDirs := a.CreateDirs == nil || *a.CreateDirs
	if !createDirs {
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			return fmt.Errorf("write_file: parent directory does not exist (set create_dirs=true to create it): %w", err)
		}
	}
	return nil
}

// writeFileCapture reads the pre-write content once and derives both the diff
// baseline (oldContent, gated on show_write_diff and the 200 KiB diff cap) and
// the undo snapshot (undoBefore + undoOK, gated on the undo store being wired
// and the 1 MiB snapshot cap). A new file needs no read: oldContent is empty
// and the write is undoable by deletion. A read failure or an over-cap file
// disables undo for this write rather than erroring.
func (t *WriteFile) writeFileCapture(path string, isNew bool) (oldContent, undoBefore string, undoOK bool) {
	wantDiff := t.deps.showWriteDiff()
	wantUndo := t.deps.Undo != nil
	if isNew {
		return "", "", wantUndo
	}
	if !wantDiff && !wantUndo {
		return "", "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	s := string(b)
	if wantDiff && len(b) <= 200*1024 {
		oldContent = s
	}
	if wantUndo && len(b) <= maxUndoSnapshotBytes {
		undoBefore, undoOK = s, true
	}
	return oldContent, undoBefore, undoOK
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
	t.deps.recordWritten(path)
}

func (t *WriteFile) formatWriteFileResult(path, newContent, oldContent string, isNew bool, uri string, awaitFresh bool) string {
	verb := "updated"
	if isNew {
		verb = "created"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s %s", verb, path, sizeSummary(newContent))
	if t.deps.showWriteDiff() {
		if isNew {
			sb.WriteString("\nnew file")
		} else if d := unifiedDiff(path, oldContent, newContent); d != "" {
			sb.WriteString("\n")
			sb.WriteString(d)
		}
	}
	sb.WriteString(t.deps.postWriteDiagnostics(uri, newContent, awaitFresh))
	return sb.String()
}
