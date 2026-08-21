package tools

// transaction_diag.go — await_diagnostics and fail_on_new_errors for
// transaction_apply (PLAN-362 PR2).
//
// The single-file tools decide per file; a transaction is ALL-OR-NOTHING, which
// is the whole reason to use one: a coordinated multi-file change is only
// correct as a set. So the gate is applied to the batch — if ANY written file
// comes back with confirmed new errors, EVERY file in the transaction is
// restored to its pre-transaction content.
//
// Ordering matters. The durable rollback log (txlog) is normally committed at
// the end of the write phase; when fail_on_new_errors is set it is HELD OPEN
// across the diagnostics gate instead, so a crash mid-gate leaves an orphan log
// that replays back to the pre-transaction state — the same answer the gate
// would have given. It is committed only once the batch is accepted.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/tools/txlog"
)

// txFileDiag is one written file's post-write diagnostics result.
type txFileDiag struct {
	path string
	diag postWriteDiagResult
}

// txDiagReport is the batch's post-write diagnostics: one entry per written
// file, plus the rendered block appended to the response.
type txDiagReport struct {
	files []txFileDiag
}

// anyNewErrors reports whether ANY file in the batch has CONFIRMED new errors —
// the all-or-nothing gate. Unconfirmed results never count, exactly as in the
// single-file case: a transaction is not rolled back on uncertainty.
func (r txDiagReport) anyNewErrors() bool {
	for _, f := range r.files {
		if f.diag.delta.hasNewErrors() {
			return true
		}
	}
	return false
}

// text renders the per-file blocks, each headed by its path so a multi-file
// response stays readable.
func (r txDiagReport) text() string {
	var sb strings.Builder
	for _, f := range r.files {
		if f.diag.text == "" {
			continue
		}
		fmt.Fprintf(&sb, "\n%s:%s", f.path, f.diag.text)
	}
	return sb.String()
}

// txDiagOpts derives the diagnostics request for the batch.
func (a transactionApplyArgs) txDiagOpts(lspNotifyFailed bool) postWriteDiagOpts {
	confirm := a.AwaitDiagnostics || a.FailOnNewErrors
	return postWriteDiagOpts{
		awaitFresh:      confirm,
		structured:      confirm,
		lspNotifyFailed: lspNotifyFailed,
	}
}

// txCaptureBaselines snapshots the pre-write language-server state for every
// operation, BEFORE the write phase mutates anything. Only taken when the caller
// asked for a confirmed answer — the default transaction pays nothing.
func (t *TransactionApply) txCaptureBaselines(a transactionApplyArgs, prepared []txPrepared) map[string]*diagBaseline {
	if !a.AwaitDiagnostics && !a.FailOnNewErrors {
		return nil
	}
	out := make(map[string]*diagBaseline, len(prepared))
	for _, p := range prepared {
		out[p.path] = t.deps.capturePreWriteBaseline("file://" + p.path)
	}
	return out
}

// txPostWriteDiagnostics runs the post-write pass over every written file. It is
// serial and each file may wait for the language server, so it runs only when
// the caller asked for it.
func (t *TransactionApply) txPostWriteDiagnostics(a transactionApplyArgs, written []txPrepared, baselines map[string]*diagBaseline, notifyFailed map[string]bool) txDiagReport {
	if !a.AwaitDiagnostics && !a.FailOnNewErrors {
		return txDiagReport{}
	}
	rep := txDiagReport{files: make([]txFileDiag, 0, len(written))}
	for _, p := range written {
		uri := "file://" + p.path
		rep.files = append(rep.files, txFileDiag{
			path: p.path,
			diag: t.deps.postWriteDiagnostics(uri, p.before, p.after, a.txDiagOpts(notifyFailed[p.path]), baselines[p.path]),
		})
	}
	return rep
}

// txRollbackNewErrors restores every file in the batch and returns the refusal.
// It always returns a non-nil error: reaching it means the caller asked for the
// transaction to be refused if it broke the build, and it did.
//
// The restore is VERIFIED per file: a file whose on-disk content no longer
// matches what this transaction wrote was changed by something outside plumb
// while the locks were held, and restoring it would discard that change — so it
// is reported, not reverted. The durable log is then committed (which only
// removes it), never replayed: replaying would restore unconditionally and
// undo exactly the change the verification chose to preserve.
func (t *TransactionApply) txRollbackNewErrors(ctx context.Context, written []txPrepared, txl *txlog.Log, rep txDiagReport) error {
	restored, skipped := rollbackVerified(written)
	txl.Commit()
	t.txNotifyRestored(ctx, written, restored)

	var sb strings.Builder
	fmt.Fprintf(&sb, "transaction_apply: refused — %d of %d written %s introduced new errors, so the whole transaction was ROLLED BACK (fail_on_new_errors).\n",
		txFilesWithNewErrors(rep), len(written), textfmt.Plural(len(written), "file", "files"))
	for _, f := range rep.files {
		if !f.diag.delta.hasNewErrors() {
			continue
		}
		fmt.Fprintf(&sb, "  %s:\n%s", f.path, renderNewErrorList(f.diag.delta))
	}
	fmt.Fprintf(&sb, "restored %d %s to their pre-transaction content.\n", len(restored), textfmt.Plural(len(restored), "file", "files"))
	if len(skipped) > 0 {
		fmt.Fprintf(&sb, "NOT restored (changed outside plumb during this call — reverting would discard that change):\n  %s\n",
			strings.Join(skipped, "\n  "))
	}
	sb.WriteString("Fix the cause and retry, or drop fail_on_new_errors to land the change anyway.")
	sb.WriteString(rep.text())
	return &editLogicErr{fmt.Errorf("%s", sb.String())}
}

func txFilesWithNewErrors(rep txDiagReport) int {
	n := 0
	for _, f := range rep.files {
		if f.diag.delta.hasNewErrors() {
			n++
		}
	}
	return n
}

// txNotifyRestored tells the language server, caches and trackers about the
// files the rollback actually restored — a file left alone by the verification
// still holds what the transaction wrote, so it must NOT be announced as
// reverted.
func (t *TransactionApply) txNotifyRestored(ctx context.Context, written []txPrepared, restored []string) {
	restoredSet := make(map[string]bool, len(restored))
	for _, p := range restored {
		restoredSet[p] = true
	}
	for _, p := range written {
		if !restoredSet[p.path] {
			continue
		}
		t.deps.notifyReverted(ctx, p.path, "file://"+p.path, protocol.FileChanged)
	}
}

// rollbackVerified restores each written file to its pre-transaction content,
// but only when the file still holds exactly what this transaction wrote. It
// returns the paths restored and the paths deliberately left alone.
//
// The plain rollback() used inside the write phase needs no such check: nothing
// has been acknowledged yet and the window is microseconds. This one runs after
// a bounded wait for a language server, so an external process has had real time
// to write — and silently reverting someone else's change is the one outcome a
// safety feature must not produce.
func rollbackVerified(written []txPrepared) (restored, skipped []string) {
	for _, p := range written {
		cur, err := os.ReadFile(p.path)
		if err != nil {
			slog.Error("transaction_apply: rollback could not read file back", "path", p.path, "err", err)
			skipped = append(skipped, p.path)
			continue
		}
		if sha256OfString(string(cur)) != sha256OfString(p.after) {
			slog.Warn("transaction_apply: rollback skipped — file changed since this transaction wrote it", "path", p.path)
			skipped = append(skipped, p.path)
			continue
		}
		if _, err := safeWrite(p.path, []byte(p.before), p.perm); err != nil {
			slog.Error("transaction_apply: rollback failed", "path", p.path, "err", err)
			skipped = append(skipped, p.path)
			continue
		}
		restored = append(restored, p.path)
	}
	return restored, skipped
}
