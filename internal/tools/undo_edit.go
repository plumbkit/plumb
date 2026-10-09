package tools

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

var undoEditSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "file_path":{"type":"string","description":"File to revert (absolute, file:// URI, or workspace-relative)."},
    "force":{"type":"boolean","description":"Revert even if the file changed since plumb's last write (default false: refused, so another's change is not discarded)."}
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

// undoEditContested is the refusal for undo_edit on a connection whose pin is
// contested. The undo snapshot plumb would restore is keyed per connection, not
// per project, so on a connection whose pin is being fought over it cannot be
// attributed to the agent that wrote it — undoing could revert a peer's most
// recent write.
const undoEditContested = "undo_edit: this connection's workspace pin is contested (several agents are multiplexing this plumb serve without declaring an identity), so the undo snapshot cannot be attributed to the agent that wrote it and may revert a peer's most recent write. Refused rather than clobber the wrong agent's work. Identify the agents (" + PerCallIdentityRemedy + "), then re-edit the file with an absolute path instead"

func NewUndoEdit(deps WriteDeps) *UndoEdit { return &UndoEdit{deps: deps} }

func (*UndoEdit) Name() string                 { return "undo_edit" }
func (*UndoEdit) InputSchema() json.RawMessage { return undoEditSchema }
func (*UndoEdit) Description() string {
	return "Revert plumb's most recent write to a file: the safe alternative to git checkout, which discards EVERY uncommitted change. Restores only what the last edit_file or write_file changed, and refuses if the file has changed since (force overrides); a write that created the file is undone by removing it. One level per file, per session, cleared on a workspace switch; not available when the pre-write content exceeded 1 MiB. Returns a diff of the undo."
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
		return undoEditArgs{}, errors.New("undo_edit: file_path is required")
	}
	return undoEditArgs{Path: in.Path, Force: in.Force}, nil
}

func (t *UndoEdit) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseUndoEditArgs(raw)
	if err != nil {
		return "", err
	}
	if t.deps.Contested != nil && t.deps.Contested() {
		return "", errors.New(undoEditContested)
	}
	path, err := t.deps.resolvePath(ctx, a.Path)
	if err != nil {
		return "", fmt.Errorf("undo_edit: %w", err)
	}
	if err := t.deps.checkBoundary(ctx, path); err != nil {
		return "", fmt.Errorf("undo_edit: %w", err)
	}

	unlock := lockPath(path)
	defer unlock()

	undo := t.deps.undo(ctx)
	if undo == nil {
		return "", fmt.Errorf("undo_edit: nothing to undo for %q — plumb has recorded no revertible write to it this session", path)
	}
	snap, ok := undo.Peek(path)
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
	undo.Take(path)
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
			return staleOverride(fmt.Errorf("undo_edit: refusing to undo — %q no longer exists (deleted or moved since plumb wrote it); pass force:true to restore it anyway", path))
		}
		return fmt.Errorf("undo_edit: reading %q: %w", path, err)
	}
	if sha256OfString(string(cur)) != snap.afterSHA {
		return staleOverride(fmt.Errorf("undo_edit: refusing to undo — %q changed since plumb's %s wrote it (an external or peer edit), so undoing would discard that change; pass force:true to override", path, snap.tool))
	}
	return nil
}

// applyUndo performs the revert: deleting a file the write created, or restoring
// the pre-write content otherwise.
func (t *UndoEdit) applyUndo(ctx context.Context, path string, snap undoSnapshot) (string, error) {
	uri := "file://" + path
	wrote, _ := hex.DecodeString(snap.afterSHA)
	if !snap.existedBefore {
		// contentSide, not historySide: the removed bytes are the response diff
		// as well as the history row (see wantContent).
		cur := t.deps.contentSide(path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("undo_edit: removing %q: %w", path, err)
		}
		t.deps.recordHistory(ctx, history.Change{
			Op:         history.OpRevert,
			Tool:       "undo_edit",
			Path:       path,
			Before:     cur,
			RevertsSHA: wrote,
			Reason:     "undo_edit",
		})
		t.notifyUndo(ctx, path, uri, protocol.FileDeleted)
		var sb strings.Builder
		fmt.Fprintf(&sb, "undid %s: removed %s (it had been newly created)", snap.tool, path)
		// The removal is a real change to the file's content, so it gets the same
		// diff treatment as a delete_file: every line the undo took away.
		diff := t.deps.responseDiff(ctx, path, sideOf(cur), absentSide())
		appendSections(&sb, t.deps.relayNoteFor(diff), diff)
		return sb.String(), nil
	}

	// contentSide, not a bare ReadFile: a force undo of a file deleted since
	// plumb wrote it must record and show an ABSENT before-side, not an existing
	// empty file.
	current := t.deps.contentSide(path)
	perm := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0 {
		perm = info.Mode().Perm()
	}
	res, err := safeWrite(path, []byte(snap.before), perm)
	if err != nil {
		return "", fmt.Errorf("undo_edit: %w", err)
	}
	t.deps.recordHistory(ctx, history.Change{
		Op:         history.OpRevert,
		Tool:       "undo_edit",
		Path:       path,
		Before:     current,
		After:      history.SideFromBytes([]byte(snap.before)),
		RevertsSHA: wrote,
		Reason:     "undo_edit",
	})
	t.notifyUndo(ctx, path, uri, protocol.FileChanged)
	t.deps.recordWritten(ctx, path, res.written)
	t.deps.notifyTopology(path)
	return t.formatUndoRestore(ctx, path, current, snap), nil
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

func (t *UndoEdit) formatUndoRestore(ctx context.Context, path string, current history.Side, snap undoSnapshot) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "undid %s: restored %s %s", snap.tool, path, sizeSummary(snap.before))
	// The restored side is in hand; the replaced side is whatever contentSide
	// read, absent when the file had been deleted.
	diff := t.deps.responseDiff(ctx, path, sideOf(current), presentSide(snap.before))
	appendSections(&sb, t.deps.relayNoteFor(diff), diff)
	return sb.String()
}
