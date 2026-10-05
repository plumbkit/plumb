package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

var renameFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "from": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the source file."
    },
    "to": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the destination file. Parent directories are created automatically."
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
  "required": ["from", "to"],
  "additionalProperties": false
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
	return "Move (rename) a file. " +
		"Parent directories of `to` are created if missing. " +
		"Refuses to overwrite an existing destination unless overwrite=true. The LSP server " +
		"is notified with FileDeleted (source) and FileCreated (destination) so symbol " +
		"indexes and diagnostics update immediately. " +
		"A rename changes no content, so the response shows a diff only when the move destroyed an " +
		"existing destination (that destination's content), gated by [edits].show_write_diff. " +
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
	if !t.deps.limiter(ctx).Allow() {
		return "", rateLimitError("rename_file", t.deps.limiter(ctx))
	}
	a, err := parseRenameFileArgs(raw)
	if err != nil {
		return "", err
	}
	from, err := t.deps.resolvePath(ctx, a.From)
	if err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}
	to, err := t.deps.resolvePath(ctx, a.To)
	if err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}
	// Same-place by canonical PATH, not raw spelling: renaming a file onto
	// itself under a second spelling is a no-op request that must be refused —
	// left alone, lockPaths collapses both spellings into one lock and the call
	// would proceed as a self-rename. Under the old keying (two spellings, two
	// keys, raw-sorted double locking) this exact shape was a self-deadlock on
	// one non-reentrant mutex, no concurrency needed.
	//
	// paths.Canonical, deliberately NOT the folding lockPathKey: the question
	// here is whether the rename would be a no-op, which is about the directory
	// ENTRY, and "file.txt" -> "FILE.txt" is not one. A case-preserving
	// filesystem stores the new spelling and `mv` performs exactly this rename,
	// so keying the check by file identity would refuse a real operation — with
	// a "same path" message naming something the caller did not ask for — and
	// leave no way to correct a file's casing through this tool. Locking below
	// still uses the folded key, so the pair takes ONE mutex and the deadlock
	// this guard was added for cannot come back through the gap.
	if paths.Canonical(from) == paths.Canonical(to) {
		return "", errors.New("rename_file: from and to are the same path")
	}
	if err := t.deps.checkBoundary(ctx, from); err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}
	if err := t.deps.checkBoundary(ctx, to); err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}

	// Lock both paths ordered by canonical lock key, never raw spelling: two
	// concurrent calls naming one path set by different spellings must acquire
	// the same keys in the same order, or they cycle.
	unlocks := lockPaths([]string{from, to})
	defer unlockAll(unlocks)

	if err := renameFilePreconditions(ctx, t.deps, from, to, a); err != nil {
		return "", err
	}
	// `to` may already name the source itself: a case-only rename on a
	// case-insensitive volume, or a second hard link. Neither destroys a
	// destination. Between two hard links rename(2) does nothing at all, so
	// reporting success would record a move that never happened.
	same := sameFileEntry(from, to)
	if same && !strings.EqualFold(from, to) {
		return "", fmt.Errorf("rename_file: %q and %q are hard links to the same file, and "+
			"renaming one onto the other leaves both in place; delete_file the name you no longer want", from, to)
	}
	// The version the move publishes at `to`, read from the source through one
	// descriptor BEFORE the rename: a rename moves the inode — bytes and mtime
	// intact — so this is what `to` holds when it lands. rename_file writes no
	// bytes of its own, and re-reading `to` afterwards would record whatever an
	// outside writer had put there by then as this session's version (issue #528).
	// A source this snapshot cannot read leaves the version unknown, which records
	// no read state rather than a guessed one.
	moved, _ := readSnapshot(from, func(io.Reader) error { return nil })
	// contentSide for the DESTINATION only: its bytes are what the response diff
	// shows when this move destroys it. The source side feeds the history row
	// alone, so it stays on historySide — reading up to 8 MiB for a diff that
	// never renders it would be waste on every rename with history off.
	src := t.deps.historySide(from)
	var dest history.Side
	if !same {
		dest = t.deps.contentSide(to)
	}
	if err := os.Rename(from, to); err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}
	if dest.Exists {
		t.deps.recordHistory(ctx, history.Change{
			Op:     history.OpDelete,
			Tool:   "rename_file",
			Path:   to,
			Before: dest,
		})
	}
	t.deps.recordHistory(ctx, history.Change{
		Op:     history.OpRename,
		Tool:   "rename_file",
		Path:   to,
		From:   from,
		Before: src,
		After:  src,
	})
	// Fsync both parent directories so the move survives a hard crash: the
	// source dir loses an entry, the destination dir gains one (they differ
	// whenever the move crosses directories).
	syncDirBestEffort("rename_file", filepath.Dir(from))
	if filepath.Dir(to) != filepath.Dir(from) {
		syncDirBestEffort("rename_file", filepath.Dir(to))
	}
	t.renameFilePostRename(ctx, from, to, moved)
	return t.formatRenameResult(ctx, from, to, dest), nil
}

// formatRenameResult renders the response. A rename does not change content, so
// there is nothing for a diff to show unless the move DESTROYED an existing
// destination — and that destination's content appears nowhere else in the
// transcript, which is what makes it worth showing.
func (t *RenameFile) formatRenameResult(ctx context.Context, from, to string, dest history.Side) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "renamed %s → %s", from, to)
	if !dest.Exists {
		return sb.String()
	}
	// The pair goes to the gate as one change, as the store classifies it.
	diff := t.deps.responseDiffAcross(ctx, to, from, sideOf(dest), absentSide())
	appendSections(&sb, t.deps.relayNoteFor(diff), diff)
	return sb.String()
}

// sameFileEntry reports whether from and to are one file. Lstat, not Stat: a
// symlink at `to` pointing at the source is a separate entry that the rename
// really does replace.
func sameFileEntry(from, to string) bool {
	fi, err := os.Lstat(from)
	if err != nil {
		return false
	}
	ti, err := os.Lstat(to)
	return err == nil && os.SameFile(fi, ti)
}

func parseRenameFileArgs(raw json.RawMessage) (renameFileArgs, error) {
	var a renameFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("rename_file: invalid arguments: %w", err)
	}
	if a.From == "" || a.To == "" {
		return a, errors.New("rename_file: both `from` and `to` are required")
	}
	if paths.URIToPath(a.From) == paths.URIToPath(a.To) {
		return a, errors.New("rename_file: from and to are the same path")
	}
	return a, nil
}

func renameFilePreconditions(ctx context.Context, deps WriteDeps, from, to string, a renameFileArgs) error {
	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("rename_file: source: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("rename_file: %q is a directory — refusing to move recursively", from)
	}
	if !a.DirtyOk && dirtyBlocksMove(ctx, deps, from) {
		return dirtyWrite(fmt.Errorf("rename_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", from))
	}
	if !a.Overwrite {
		if _, err := os.Stat(to); err == nil {
			return fmt.Errorf("rename_file: destination %q exists (pass overwrite=true to replace)", to)
		}
	}
	// Synced: a newly created destination tree must be as durable as the move
	// itself, or a crash can lose both despite the acknowledgement.
	if err := mkdirAllSynced("rename_file", filepath.Dir(to)); err != nil {
		return fmt.Errorf("rename_file: creating parent dirs: %w", err)
	}
	return nil
}

func (t *RenameFile) renameFilePostRename(ctx context.Context, from, to string, moved fileSnapshot) {
	if err := notifyLSP(ctx, t.deps.Client, from, protocol.FileDeleted); err != nil {
		slog.Warn("rename_file: LSP delete-notify failed", "path", from, "err", err)
	}
	if err := notifyLSP(ctx, t.deps.Client, to, protocol.FileCreated); err != nil {
		slog.Warn("rename_file: LSP create-notify failed", "path", to, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+from)
	invalidateCache(t.deps.Cache, "file://"+to)
	// Enqueue from first: processUpsert detects the missing file and routes to
	// processDelete. Then enqueue to so the new path is indexed immediately.
	t.deps.notifyTopology(from)
	t.deps.notifyTopology(to)
	t.deps.recordWritten(ctx, to, moved)
}
