package tools

// fail_on_new_errors.go — the refuse-to-break-the-build half of PLAN-362.
//
// With fail_on_new_errors:true a write that the language server confirms
// introduced NEW errors is ROLLED BACK: the file ends the call byte-for-byte as
// it began, and the call returns the structured delta as a refusal. It is the
// one thing a patch-application tool cannot do — but only if it is true, so the
// rules below are deliberately narrow.
//
// WHEN A ROLLBACK HAPPENS. Exactly one condition: the post-write pass was
// CONFIRMED fresh for the edited file (diagDelta.Fresh) AND it reports at least
// one new ERROR in that file (diagDelta.NewErrors). Everything else lands:
//
//   - Unconfirmed freshness — the wait expired, the window is disabled, the LSP
//     notification failed, no diagnostics source, a failed pull. Never roll back
//     on uncertainty: an agent whose good edit is silently reverted learns to
//     stop using the flag, which is worse than not having it.
//   - Warnings. Only errors block, per the card.
//   - Pre-existing errors. The delta is against the pre-write baseline, so a
//     file that was already broken can still be edited.
//   - The re-index-lag class (`stale?`), which splitDifferential separates out —
//     these are precisely the phantom errors an edit usually RESOLVES.
//   - New errors in OTHER files (cross-file sweep). They are reported in the
//     delta, but they are attributed by publish-time heuristics, the sweep is off
//     by default and non-exhaustive in pull mode, and a mid-refactor edit breaks
//     dependents on purpose. Rolling back on them would revert good work on weak
//     evidence.
//
// ROLLBACK IS A WRITE. It goes through safeWrite — the same atomic
// temp+fsync+rename path as every other write in the tree — never a bare
// os.WriteFile, so a crash during the revert cannot leave a half-file.
//
// IT MUST NOT CLOBBER A PEER. Before restoring, the on-disk content is compared
// against what plumb wrote. A mismatch means someone else's write landed in
// between: plumb reports that instead of reverting, exactly as undo_edit does.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// failOnNewErrorsSizeLimit is the pre-write content size above which
// fail_on_new_errors is refused up-front. It is maxUndoSnapshotBytes by
// construction: a write plumb cannot snapshot for undo_edit is a write whose
// revert would rest on nothing but the in-memory copy, and a tool that promises
// "the file is unchanged if this fails" must not make that promise on a file
// whose recovery path it has already declined to keep.
const failOnNewErrorsSizeLimit = maxUndoSnapshotBytes

// failOnNewErrorsPrecheck refuses a fail_on_new_errors write on a file too large
// to snapshot, BEFORE anything is written. Refusing up-front is the point: the
// alternative is discovering at rollback time that the safety net was never
// there, with the broken content already on disk.
func failOnNewErrorsPrecheck(tool, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // a file that does not exist yet is created by this write; nothing to snapshot
	}
	if info.Size() > failOnNewErrorsSizeLimit {
		return badArgument(fmt.Errorf(
			"%s: fail_on_new_errors is not available for %q (%d bytes, over the %d-byte snapshot cap) — "+
				"plumb keeps no revertible snapshot of a file this large, so it cannot promise to restore it. "+
				"Retry without fail_on_new_errors and check the result, or split the file",
			tool, path, info.Size(), failOnNewErrorsSizeLimit))
	}
	return nil
}

// rollbackRequest is everything the revert needs about the write it is undoing.
type rollbackRequest struct {
	tool          string
	path          string
	uri           string
	before        string // content before the write; "" when the write created the file
	existedBefore bool
	wrote         string // exactly what plumb wrote — the concurrent-writer guard compares against this
	diag          postWriteDiagResult
}

// rollbackNewErrors reverts a write that introduced new errors and returns the
// refusal the tool hands back. It always returns a non-nil error: reaching it
// means the caller asked for the write to be refused and it has been.
//
// The caller MUST still hold the per-path write lock — this runs inside the same
// locked region as the write and the analysis, so no other plumb writer can
// interleave between deciding and reverting.
func (d WriteDeps) rollbackNewErrors(ctx context.Context, req rollbackRequest) error {
	if err := d.revertWrite(ctx, req); err != nil {
		return fmt.Errorf("%s: this write introduced %s and was refused, but it could NOT be rolled back: %w\n"+
			"The file is still in its post-write state — revert it yourself before continuing.%s",
			req.tool, newErrorsPhrase(req.diag.delta), err, req.diag.text)
	}
	// The undo snapshot this write armed now points at a write that no longer
	// exists on disk. Take it rather than leave it: an undo_edit against it would
	// refuse anyway (the content no longer matches what plumb wrote), and a stale
	// armed entry is worse than an honest "nothing to undo".
	d.undo(ctx).Take(req.path)
	return fmt.Errorf("%s: refused — %s, so the write was ROLLED BACK and %s is byte-for-byte unchanged (fail_on_new_errors).\n"+
		"%sFix the cause and retry, or drop fail_on_new_errors to land the change anyway.%s",
		req.tool, newErrorsPhrase(req.diag.delta), req.path,
		renderNewErrorList(req.diag.delta), req.diag.text)
}

// revertWrite restores the file, or explains why it would not. A write that
// CREATED the file is undone by removing it; otherwise the pre-write content is
// written back through safeWrite, so the revert is as atomic and as durable as
// the write it undoes.
func (d WriteDeps) revertWrite(ctx context.Context, req rollbackRequest) error {
	cur, err := os.ReadFile(req.path)
	if err != nil {
		if os.IsNotExist(err) && !req.existedBefore {
			return nil // already gone; the state we wanted
		}
		return fmt.Errorf("reading %q back: %w", req.path, err)
	}
	if sha256OfString(string(cur)) != sha256OfString(req.wrote) {
		return fmt.Errorf(
			"%q no longer holds what plumb wrote — another process modified it during this call, and reverting would discard that change",
			req.path)
	}
	if !req.existedBefore {
		if err := os.Remove(req.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %q: %w", req.path, err)
		}
		d.notifyReverted(ctx, req.path, req.uri, protocol.FileDeleted)
		return nil
	}
	perm := os.FileMode(0o644)
	if info, statErr := os.Stat(req.path); statErr == nil && info.Mode().Perm() != 0 {
		perm = info.Mode().Perm()
	}
	if _, err := safeWrite(req.path, []byte(req.before), perm); err != nil {
		return fmt.Errorf("restoring %q: %w", req.path, err)
	}
	d.notifyReverted(ctx, req.path, req.uri, protocol.FileChanged)
	return nil
}

// notifyReverted mirrors the post-write notification, so the language server,
// the symbol cache, the topology index and this session's own read/write state
// all see the restored content rather than the reverted one.
func (d WriteDeps) notifyReverted(ctx context.Context, path, uri string, ct protocol.FileChangeType) {
	if err := notifyLSP(ctx, d.Client, path, ct); err != nil {
		slog.Warn("fail_on_new_errors: LSP notification after rollback failed", "path", path, "err", err)
	}
	if d.PostWriteNotifyFn != nil {
		if err := d.PostWriteNotifyFn(ctx, path); err != nil {
			slog.Warn("fail_on_new_errors: post-write adapter notification after rollback failed", "path", path, "err", err)
		}
	}
	invalidateCache(d.Cache, uri)
	if ct != protocol.FileDeleted {
		d.recordWritten(ctx, path)
	}
	d.notifyTopology(path)
}

// newErrorsPhrase renders the count for the refusal sentence.
func newErrorsPhrase(delta diagDelta) string {
	n := len(delta.NewErrors) + delta.OmittedNewErrors
	if n == 1 {
		return "1 new error"
	}
	return fmt.Sprintf("%d new errors", n)
}

// renderNewErrorList lists the offending errors, so the caller can act without a
// second call.
func renderNewErrorList(delta diagDelta) string {
	var sb strings.Builder
	for _, e := range delta.NewErrors {
		fmt.Fprintf(&sb, "  error L%d: %s\n", e.Line, e.Message)
	}
	if delta.OmittedNewErrors > 0 {
		fmt.Fprintf(&sb, "  …(+%d more)\n", delta.OmittedNewErrors)
	}
	return sb.String()
}
