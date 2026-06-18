package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

var undoEditSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "file_path":{"type":"string","description":"Absolute path, file:// URI, or workspace-relative path of the file to revert."},
    "force":{"type":"boolean","description":"Revert even if the file changed since plumb's last write to it (an external or peer edit). Default false — the undo is refused in that case so it cannot silently discard someone else's change."}
  },
  "required":["file_path"],
  "additionalProperties":false
}`)

// UndoEdit reverts plumb's most recent write to a single file, using the
// per-session UndoStore snapshot. It is the safe counterpart to a whole-file
// `git checkout`/`git restore`, which discards every uncommitted change in the
// file: undo_edit restores only what plumb's last write changed and refuses, by
// default, when the file has changed since (so a peer's edit is never silently
// clobbered).
//
// Concurrency: Execute is safe for concurrent use; it takes the per-path write
// lock for the duration of the revert.
type UndoEdit struct {
	deps WriteDeps
}

func NewUndoEdit(deps WriteDeps) *UndoEdit { return &UndoEdit{deps: deps} }

func (*UndoEdit) Name() string                 { return "undo_edit" }
func (*UndoEdit) InputSchema() json.RawMessage { return undoEditSchema }
func (*UndoEdit) Description() string {
	return "Revert plumb's most recent write to a file — the safe alternative to `git checkout <file>`, which discards EVERY uncommitted change in the file. " +
		"undo_edit restores only what plumb's last edit_file/write_file changed, and refuses by default if the file was modified since (an external or peer edit), so it never silently clobbers someone else's work (pass force:true to override). " +
		"If the last write created the file, undo removes it. Single-level per file: it undoes the last write; a fresh write re-arms it. Undo history is per session and cleared on a workspace switch. " +
		"Very large files (pre-write content over 1 MiB) are not snapshotted, so undo is unavailable for them."
}

type undoEditArgs struct {
	Path  string
	Force bool
}

func parseUndoEditArgs(raw json.RawMessage) (undoEditArgs, error) {
	var in struct {
		Path  string `json:"file_path"`
		Force bool   `json:"force"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return undoEditArgs{}, fmt.Errorf("undo_edit: invalid arguments: %w", err)
	}
	if in.Path == "" {
		return undoEditArgs{}, fmt.Errorf("undo_edit: file_path is required")
	}
	return undoEditArgs{Path: in.Path, Force: in.Force}, nil
}

func (t *UndoEdit) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseUndoEditArgs(raw)
	if err != nil {
		return "", err
	}
	path := t.deps.resolvePath(a.Path)
	if err := t.deps.checkBoundary(path); err != nil {
		return "", fmt.Errorf("undo_edit: %w", err)
	}

	unlock := lockPath(path)
	defer unlock()

	snap, ok := t.deps.Undo.Peek(path)
	if !ok {
		return "", fmt.Errorf("undo_edit: nothing to undo for %q — plumb has recorded no revertible write to it this session", path)
	}
	if err := t.checkUndoSafe(path, snap, a.Force); err != nil {
		return "", err
	}
	out, err := t.applyUndo(ctx, path, snap)
	if err != nil {
		return "", err
	}
	t.deps.Undo.Take(path)
	return out, nil
}

// checkUndoSafe refuses the undo when the file has diverged from what plumb
// wrote (an external edit), unless force is set — so an undo never silently
// discards a peer's change. A force undo skips the check entirely.
func (t *UndoEdit) checkUndoSafe(path string, snap undoSnapshot, force bool) error {
	if force {
		return nil
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("undo_edit: refusing to undo — %q no longer exists (deleted or moved since plumb wrote it); pass force:true to restore it anyway", path)
		}
		return fmt.Errorf("undo_edit: reading %q: %w", path, err)
	}
	if sha256OfString(string(cur)) != snap.afterSHA {
		return fmt.Errorf("undo_edit: refusing to undo — %q changed since plumb's %s wrote it (an external or peer edit), so undoing would discard that change; pass force:true to override", path, snap.tool)
	}
	return nil
}

// applyUndo performs the revert: deleting a file the write created, or restoring
// the pre-write content otherwise.
func (t *UndoEdit) applyUndo(ctx context.Context, path string, snap undoSnapshot) (string, error) {
	uri := "file://" + path
	if !snap.existedBefore {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("undo_edit: removing %q: %w", path, err)
		}
		t.notifyUndo(ctx, path, uri, protocol.FileDeleted)
		return fmt.Sprintf("undid %s: removed %s (it had been newly created)", snap.tool, path), nil
	}

	current, _ := os.ReadFile(path) // best-effort, for the diff only
	perm := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0 {
		perm = info.Mode().Perm()
	}
	if _, err := safeWrite(path, []byte(snap.before), perm); err != nil {
		return "", fmt.Errorf("undo_edit: %w", err)
	}
	t.notifyUndo(ctx, path, uri, protocol.FileChanged)
	t.deps.recordWritten(path)
	t.deps.notifyTopology(path)
	return t.formatUndoRestore(path, string(current), snap), nil
}

// notifyUndo mirrors the post-write notification the write tools perform, so the
// LSP server and symbol cache see the reverted content immediately.
func (t *UndoEdit) notifyUndo(ctx context.Context, path, uri string, ct protocol.FileChangeType) {
	if err := notifyLSP(ctx, t.deps.Client, path, ct); err != nil {
		slog.Warn("undo_edit: LSP notification failed", "path", path, "err", err)
	}
	if t.deps.PostWriteNotifyFn != nil {
		if err := t.deps.PostWriteNotifyFn(ctx, path); err != nil {
			slog.Warn("undo_edit: post-write adapter notification failed", "path", path, "err", err)
		}
	}
	invalidateCache(t.deps.Cache, uri)
}

func (t *UndoEdit) formatUndoRestore(path, current string, snap undoSnapshot) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "undid %s: restored %s %s", snap.tool, path, sizeSummary(snap.before))
	if t.deps.showWriteDiff() {
		if d := unifiedDiff(path, current, snap.before); d != "" {
			sb.WriteString("\n")
			sb.WriteString(d)
		}
	}
	return sb.String()
}
