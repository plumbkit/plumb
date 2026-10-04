package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

var copyFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "from": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the source file."
    },
    "to": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the destination. Parent directories are created automatically."
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
		"The response shows the diff of what landed at the destination — every line added, plus the " +
		"destination it replaced when overwrite=true — gated by [edits].show_write_diff. " +
		"To move or rename a file, use rename_file instead."
}

type copyFileArgs struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
	DirtyOk   bool   `json:"dirty_ok"`
}

func (t *CopyFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.limiter(ctx).Allow() {
		return "", rateLimitError("copy_file", t.deps.limiter(ctx))
	}
	a, err := parseCopyFileArgs(raw)
	if err != nil {
		return "", err
	}
	from, err := t.deps.resolvePath(ctx, a.From)
	if err != nil {
		return "", fmt.Errorf("copy_file: %w", err)
	}
	to, err := t.deps.resolvePath(ctx, a.To)
	if err != nil {
		return "", fmt.Errorf("copy_file: %w", err)
	}
	// Same-place by canonical IDENTITY, not raw spelling: two spellings of one
	// file (a symlinked parent, a platform alias) are a no-op request that must
	// be refused — left alone, lockPaths collapses both spellings into one lock
	// and the call would proceed as a self-copy. Under the old keying (two
	// spellings, two keys, raw-sorted double locking) this exact shape was a
	// self-deadlock on one non-reentrant mutex, no concurrency needed.
	if lockPathKey(from) == lockPathKey(to) {
		return "", errors.New("copy_file: from and to are the same path")
	}
	if err := t.deps.checkBoundary(ctx, from); err != nil {
		return "", fmt.Errorf("copy_file: %w", err)
	}
	if err := t.deps.checkBoundary(ctx, to); err != nil {
		return "", fmt.Errorf("copy_file: %w", err)
	}

	// Lock both paths ordered by canonical lock key, never raw spelling: two
	// concurrent calls naming one path set by different spellings must acquire
	// the same keys in the same order, or they cycle.
	unlocks := lockPaths([]string{from, to})
	defer unlockAll(unlocks)

	data, perm, err := copyFilePreconditions(ctx, t.deps, from, to, a)
	if err != nil {
		return "", err
	}
	// contentSide, not historySide: the destination's bytes feed the response
	// diff as well as the history row, so the read must happen when EITHER wants
	// them (see wantContent).
	destBefore := t.deps.contentSide(to)
	res, err := safeWrite(to, data, perm)
	if err != nil {
		return "", fmt.Errorf("copy_file: writing destination: %w", err)
	}
	t.deps.recordHistory(ctx, history.Change{
		Op:     history.OpCopy,
		Tool:   "copy_file",
		Path:   to,
		From:   from,
		Before: destBefore,
		After:  history.SideFromBytes(data),
	})
	t.copyFilePostWrite(ctx, to, res.written)
	return t.formatCopyResult(ctx, from, to, data, destBefore), nil
}

// formatCopyResult renders the response: the summary, the relay instruction when
// there is content to relay, then the diff of what landed at the destination. A
// copy onto a free path renders every line added; an overwrite additionally
// renders the destination it replaced, which is the part nothing else in the
// transcript shows.
func (t *CopyFile) formatCopyResult(ctx context.Context, from, to string, data []byte, destBefore history.Side) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "copied %s → %s (%d bytes)", from, to, len(data))
	// Both paths go to the gate: a copy of a sensitive SOURCE to a destination
	// whose name matches no glob would otherwise print the secret.
	diff := t.deps.responseDiffAcross(ctx, []string{from, to}, to, sideOf(destBefore), bytesSide(data))
	appendSections(&sb, t.deps.relayNoteFor(diff), diff)
	return sb.String()
}

func parseCopyFileArgs(raw json.RawMessage) (copyFileArgs, error) {
	var a copyFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("copy_file: invalid arguments: %w", err)
	}
	if a.From == "" || a.To == "" {
		return a, errors.New("copy_file: both `from` and `to` are required")
	}
	if paths.URIToPath(a.From) == paths.URIToPath(a.To) {
		return a, errors.New("copy_file: from and to are the same path")
	}
	return a, nil
}

// copyFilePreconditions validates the source, checks the dirty state, checks
// for destination conflicts, creates parent directories, and reads the source
// content. Returns the content and source permissions on success.
func copyFilePreconditions(ctx context.Context, deps WriteDeps, from, to string, a copyFileArgs) ([]byte, os.FileMode, error) {
	info, err := os.Stat(from)
	if err != nil {
		return nil, 0, fmt.Errorf("copy_file: source: %w", err)
	}
	if info.IsDir() {
		return nil, 0, fmt.Errorf("copy_file: %q is a directory — refusing to copy recursively", from)
	}
	if !a.DirtyOk && dirtyBlocksMove(ctx, deps, from) {
		return nil, 0, dirtyWrite(fmt.Errorf("copy_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", from))
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

func (t *CopyFile) copyFilePostWrite(ctx context.Context, to string, written fileSnapshot) {
	if err := notifyLSP(ctx, t.deps.Client, to, protocol.FileCreated); err != nil {
		slog.Warn("copy_file: LSP create-notify failed", "path", to, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+to)
	t.deps.notifyTopology(to)
	t.deps.recordWritten(ctx, to, written)
}
