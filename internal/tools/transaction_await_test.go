package tools

// transaction_await_test.go — PLAN-362 PR2 acceptance (e): a transaction whose
// batch introduces a new error in ANY written file is rolled back in FULL, and
// the reporting-only mode (await_diagnostics) leaves every file in place.

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// txDiagStub answers per URI and, like scriptedDiag, flips from the pre-write to
// the post-write set the first time the post-write wait runs for that URI.
type txDiagStub struct {
	pre     map[string][]protocol.Diagnostic
	post    map[string][]protocol.Diagnostic
	waited  map[string]bool
	waitErr error
	// onWait fires from inside the post-write wait for uri — the moment between
	// this transaction's writes and its rollback decision.
	onWait func(uri string)
}

func newTxDiagStub() *txDiagStub {
	return &txDiagStub{
		pre:    map[string][]protocol.Diagnostic{},
		post:   map[string][]protocol.Diagnostic{},
		waited: map[string]bool{},
	}
}

func (s *txDiagStub) Diagnostics(uri string) []protocol.Diagnostic {
	if s.waited[uri] {
		return s.post[uri]
	}
	return s.pre[uri]
}

func (s *txDiagStub) WaitNextDiagnostics(_ context.Context, uri string) ([]protocol.Diagnostic, error) {
	s.waited[uri] = true
	if s.onWait != nil {
		s.onWait(uri)
	}
	return s.Diagnostics(uri), s.waitErr
}

const (
	txBefore = "package p\n\nfunc a() int { return 1 }\n"
	txAfter  = "package p\n\nfunc a() int { return 2 }\n"
)

// txEnv is two seeded files and a diagnostics stub keyed by their URIs.
type txEnv struct {
	dir   string
	files []string
	diag  *txDiagStub
	deps  WriteDeps
}

func newTxEnv(t *testing.T) *txEnv {
	t.Helper()
	dir := t.TempDir()
	// txlog.Begin degrades to a no-op without a .plumb marker, which would make
	// every commit-timing probe below vacuously "committed".
	if err := os.MkdirAll(filepath.Join(dir, ".plumb"), 0o755); err != nil {
		t.Fatalf("seed .plumb: %v", err)
	}
	e := &txEnv{dir: dir, diag: newTxDiagStub()}
	for _, name := range []string{"a.go", "b.go"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(txBefore), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		e.files = append(e.files, p)
	}
	e.deps = WriteDeps{
		Diag: e.diag, Reads: NewReadTracker(), Writes: NewWriteTracker(),
		PostWriteDiagWindow: 50 * time.Millisecond,
		WorkspaceFn:         func(context.Context) string { return dir },
	}
	return e
}

func (e *txEnv) apply(t *testing.T, extra map[string]any) (string, error) {
	t.Helper()
	ops := make([]map[string]any, 0, len(e.files))
	for _, p := range e.files {
		ops = append(ops, map[string]any{
			"file_path": p,
			"edits":     []map[string]any{{"old_string": "return 1", "new_string": "return 2"}},
		})
	}
	req := map[string]any{"operations": ops}
	maps.Copy(req, extra)
	return NewTransactionApply(e.deps).Execute(context.Background(), mustJSON(req))
}

func (e *txEnv) assertAll(t *testing.T, want string) {
	t.Helper()
	for _, p := range e.files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(b) != want {
			t.Fatalf("%s\n got: %q\nwant: %q", p, b, want)
		}
	}
}

// TestTransactionApplyAwait_BatchRollsBackWhenOneOpBreaksTheBuild is acceptance
// case (e): ONE file gaining a new error rolls back EVERY file in the batch —
// all-or-nothing, which is what a transaction is for.
func TestTransactionApplyAwait_BatchRollsBackWhenOneOpBreaksTheBuild(t *testing.T) {
	e := newTxEnv(t)
	e.diag.post["file://"+e.files[1]] = []protocol.Diagnostic{
		hardErr("cannot use 2 (untyped int) as string", 2),
	}

	out, err := e.apply(t, map[string]any{"fail_on_new_errors": true})
	if err == nil {
		t.Fatalf("expected the transaction to be refused, got:\n%s", out)
	}
	// Both files — including the one that analysed clean — are back at their
	// pre-transaction content.
	e.assertAll(t, txBefore)
	msg := err.Error()
	if !strings.Contains(msg, "ROLLED BACK") {
		t.Errorf("refusal must say the transaction was rolled back, got:\n%s", msg)
	}
	if !strings.Contains(msg, "restored 2 files") {
		t.Errorf("refusal must report what it restored, got:\n%s", msg)
	}
	if !strings.Contains(msg, "cannot use 2") {
		t.Errorf("refusal must name the offending error, got:\n%s", msg)
	}
	// One delta per written file, in file order: the refusal must carry the
	// structured result for the file that broke, not just prose about it.
	deltas := parseDeltas(t, msg)
	if len(deltas) != 2 {
		t.Fatalf("expected one delta per written file, got %d", len(deltas))
	}
	found := false
	for _, d := range deltas {
		if d.Fresh && len(d.NewErrors) > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("no confirmed delta with new errors in the refusal: %+v", deltas)
	}
}

// TestTransactionApplyAwait_ReportsWithoutTheFlag: await_diagnostics alone is
// reporting, never enforcement — the batch lands and each file gets a labelled
// block with its delta.
func TestTransactionApplyAwait_ReportsWithoutTheFlag(t *testing.T) {
	e := newTxEnv(t)
	e.diag.post["file://"+e.files[1]] = []protocol.Diagnostic{
		hardErr("cannot use 2 (untyped int) as string", 2),
	}

	out, err := e.apply(t, map[string]any{"await_diagnostics": true})
	if err != nil {
		t.Fatalf("await_diagnostics alone must not fail the transaction: %v", err)
	}
	e.assertAll(t, txAfter)
	if !strings.Contains(out, postWriteDiagLabelAuthoritative) {
		t.Errorf("expected a labelled per-file block, got:\n%s", out)
	}
	if strings.Count(out, diagDeltaPrefix) != 2 {
		t.Errorf("expected one delta line per written file, got:\n%s", out)
	}
}

// TestTransactionApplyAwait_UnconfirmedNeverRollsBack: the same batch, but the
// language server never confirms. The transaction stands.
func TestTransactionApplyAwait_UnconfirmedNeverRollsBack(t *testing.T) {
	e := newTxEnv(t)
	e.diag.waitErr = context.DeadlineExceeded
	e.diag.post["file://"+e.files[1]] = []protocol.Diagnostic{
		hardErr("cannot use 2 (untyped int) as string", 2),
	}

	out, err := e.apply(t, map[string]any{"fail_on_new_errors": true})
	if err != nil {
		t.Fatalf("an unconfirmed check must not roll a transaction back: %v", err)
	}
	e.assertAll(t, txAfter)
	if !strings.Contains(out, `"fresh":false`) {
		t.Errorf("expected fresh:false in the per-file deltas, got:\n%s", out)
	}
}

// TestTransactionApplyAwait_DefaultPathPaysNothing: with neither flag, no
// baseline is captured and no wait happens — the cost is opt-in.
func TestTransactionApplyAwait_DefaultPathPaysNothing(t *testing.T) {
	e := newTxEnv(t)

	out, err := e.apply(t, nil)
	if err != nil {
		t.Fatalf("plain transaction: %v", err)
	}
	e.assertAll(t, txAfter)
	if len(e.diag.waited) != 0 {
		t.Errorf("no post-write wait should happen without await_diagnostics, waited: %v", e.diag.waited)
	}
	if strings.Contains(out, diagDeltaPrefix) {
		t.Errorf("no delta line without await_diagnostics, got:\n%s", out)
	}
}

// TestTransactionApplyAwait_ExternallyChangedFileIsReportedNotReverted: a file
// something outside plumb rewrote during the call is left alone, and the refusal
// says so — reverting it would discard that change.
func TestTransactionApplyAwait_ExternallyChangedFileIsReportedNotReverted(t *testing.T) {
	const peer = "package p // peer\n"
	e := newTxEnv(t)
	e.diag.post["file://"+e.files[0]] = []protocol.Diagnostic{
		hardErr("cannot use 2 (untyped int) as string", 2),
	}
	// Rewrite b.go from "outside" while the transaction is between its write and
	// its decision: the diagnostics wait for a.go is that moment.
	e.diag.onWait = func(uri string) {
		if uri != "file://"+e.files[0] {
			return
		}
		if err := os.WriteFile(e.files[1], []byte(peer), 0o644); err != nil {
			t.Errorf("simulating external write: %v", err)
		}
	}

	_, err := e.apply(t, map[string]any{"fail_on_new_errors": true})
	if err == nil {
		t.Fatal("expected the transaction to be refused")
	}
	if got := readFileString(t, e.files[0]); got != txBefore {
		t.Errorf("a.go should have been restored, got %q", got)
	}
	if got := readFileString(t, e.files[1]); got != peer {
		t.Errorf("the external content must survive, got %q", got)
	}
	if !strings.Contains(err.Error(), "NOT restored") {
		t.Errorf("the refusal must report the file it left alone, got:\n%s", err.Error())
	}
}

// notifyHookLSP runs a hook from inside the post-write notify pass. The
// embedded nil interface panics on any other method, so an unexpected call
// fails loudly.
type notifyHookLSP struct {
	lsp.Client
	onNotify func()
}

func (c notifyHookLSP) DidChangeWatchedFiles(context.Context, protocol.DidChangeWatchedFilesParams) error {
	if c.onNotify != nil {
		c.onNotify()
	}
	return nil
}

// txLogOpen reports whether an uncommitted transaction log exists under the
// workspace. Commit removes the log directory, so this is a direct observation
// of "has this transaction been committed yet?" — no sleeps, no timing guesses.
func txLogOpen(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, ".plumb", "tx-log"))
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// TestTransactionApplyAwait_CommitTimingIsGatedNotDelayed pins WHEN the durable
// rollback log is committed, probed from inside the notify and diagnostics
// passes.
//
// The gate holds the log open on purpose: until fail_on_new_errors has decided,
// the transaction is not accepted, and a crash mid-gate must replay back to the
// pre-transaction state. But that window must NOT be paid by a call that did not
// ask for the gate: a daemon restart during an await_diagnostics wait (seconds
// per file) would otherwise REVERT a transaction that, before this feature
// existed, had already survived. With the flag absent the behaviour must be
// exactly as before — committed the moment the writes land.
func TestTransactionApplyAwait_CommitTimingIsGatedNotDelayed(t *testing.T) {
	cases := []struct {
		name       string
		args       map[string]any
		wantOpen   bool // is the log still open during the notify pass?
		wantOpenDx bool // ...and during the diagnostics pass? (false when none runs)
	}{
		{name: "default path commits before notify", args: nil},
		{name: "await-only commits before notify", args: map[string]any{"await_diagnostics": true}},
		{name: "gated path holds the log across both", args: map[string]any{"fail_on_new_errors": true}, wantOpen: true, wantOpenDx: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTxEnv(t)
			var openAtNotify, openAtDiag, diagRan bool
			e.deps.Client = notifyHookLSP{onNotify: func() { openAtNotify = openAtNotify || txLogOpen(e.dir) }}
			e.diag.onWait = func(string) {
				diagRan = true
				openAtDiag = openAtDiag || txLogOpen(e.dir)
			}

			if _, err := e.apply(t, tc.args); err != nil {
				t.Fatalf("transaction: %v", err)
			}
			e.assertAll(t, txAfter)

			if openAtNotify != tc.wantOpen {
				t.Errorf("tx-log open during notify = %v, want %v", openAtNotify, tc.wantOpen)
			}
			if diagRan && openAtDiag != tc.wantOpenDx {
				t.Errorf("tx-log open during diagnostics = %v, want %v", openAtDiag, tc.wantOpenDx)
			}
			if txLogOpen(e.dir) {
				t.Error("the log must be committed by the time the call returns")
			}
		})
	}
}

// parseDeltas decodes every structured delta line in a multi-file response.
func parseDeltas(t *testing.T, out string) []diagDelta {
	t.Helper()
	var deltas []diagDelta
	for line := range strings.SplitSeq(out, "\n") {
		_, after, ok := strings.Cut(line, diagDeltaPrefix)
		if !ok {
			continue
		}
		var d diagDelta
		if err := json.Unmarshal([]byte(after), &d); err != nil {
			t.Fatalf("delta line is not valid JSON (%v): %s", err, line)
		}
		deltas = append(deltas, d)
	}
	return deltas
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
