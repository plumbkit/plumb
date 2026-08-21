package tools

// fail_on_new_errors_test.go — PLAN-362 PR2 acceptance: an edit the language
// server confirms broke the build is ROLLED BACK, and every other outcome
// (unconfirmed, warning-only, pre-existing, cross-file, a peer's concurrent
// write) leaves the file exactly where it landed.
//
// The tests assert BYTES on disk, not just the response text: "the file is
// unchanged" is the entire promise, and a test that only reads the message
// would pass against a rollback that never happened.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// scriptedDiag is a post-write diagnostics source whose answer CHANGES at the
// write: Diagnostics() serves pre until the post-write wait happens, then post.
// That is what "this edit introduced an error" looks like from the tool's side —
// a clean baseline, then a new finding.
//
// onWait fires from INSIDE the wait, i.e. inside the tool's per-path locked
// region: it is the callback seam the lock probe and the concurrent-writer test
// use to act at exactly that moment, instead of racing a sleep against it.
type scriptedDiag struct {
	mu       sync.Mutex
	pre      []protocol.Diagnostic
	post     []protocol.Diagnostic
	waited   bool
	waitErr  error // non-nil: the wait "timed out" — the result is not fresh
	onWait   func()
	waitCall int
}

func (s *scriptedDiag) Diagnostics(string) []protocol.Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waited {
		return s.post
	}
	return s.pre
}

func (s *scriptedDiag) WaitNextDiagnostics(_ context.Context, uri string) ([]protocol.Diagnostic, error) {
	s.mu.Lock()
	s.waited = true
	s.waitCall++
	hook := s.onWait
	err := s.waitErr
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return s.Diagnostics(uri), err
	}
	return s.Diagnostics(uri), nil
}

// notifyFailLSP fails every didChangeWatchedFiles. The embedded nil interface
// panics on any other method, so a path that reaches the server another way
// fails loudly instead of passing quietly.
type notifyFailLSP struct{ lsp.Client }

func (notifyFailLSP) DidChangeWatchedFiles(context.Context, protocol.DidChangeWatchedFilesParams) error {
	return errors.New("lsp unavailable")
}

// hardErr is a diagnostic OUTSIDE the re-index-lag class, so the differential
// treats it as genuine breakage rather than probable lag.
func hardErr(msg string, line uint32) protocol.Diagnostic {
	return protocol.Diagnostic{
		Severity: protocol.SevError,
		Message:  msg,
		Range:    protocol.Range{Start: protocol.Position{Line: line}},
	}
}

func warnAt(msg string, line uint32) protocol.Diagnostic {
	return protocol.Diagnostic{
		Severity: protocol.SevWarning,
		Message:  msg,
		Range:    protocol.Range{Start: protocol.Position{Line: line}},
	}
}

const (
	failTestBefore = "package p\n\nfunc a() int { return 1 }\n"
	failTestAfter  = "package p\n\nfunc a() int { return \"x\" }\n"
)

// failEnv is one temp file plus the tool wired to a scripted diagnostics source.
type failEnv struct {
	path string
	deps WriteDeps
	diag *scriptedDiag
	undo *UndoStore
}

func newFailEnv(t *testing.T, post []protocol.Diagnostic) *failEnv {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(failTestBefore), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	d := &scriptedDiag{post: post}
	undo := NewUndoStore()
	return &failEnv{path: path, diag: d, undo: undo, deps: WriteDeps{
		Diag: d, Undo: undo, Reads: NewReadTracker(), Writes: NewWriteTracker(),
		PostWriteDiagWindow: 50 * time.Millisecond,
	}}
}

// edit runs one str_replace edit turning failTestBefore into failTestAfter.
func (e *failEnv) edit(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	req := map[string]any{
		"file_path": e.path,
		"edits":     []map[string]any{{"old_string": "return 1", "new_string": "return \"x\""}},
	}
	for k, v := range args {
		req[k] = v
	}
	return NewEditFile(e.deps).Execute(context.Background(), mustJSON(req))
}

func (e *failEnv) content(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return string(b)
}

// parseDelta extracts and decodes the structured delta line from a response or
// error message. It is the machine-parseable contract: a caller matches the
// fixed prefix and unmarshals the rest.
func parseDelta(t *testing.T, out string) diagDelta {
	t.Helper()
	i := strings.Index(out, diagDeltaPrefix)
	if i < 0 {
		t.Fatalf("no %q line in output:\n%s", diagDeltaPrefix, out)
	}
	rest := out[i+len(diagDeltaPrefix):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	var d diagDelta
	if err := json.Unmarshal([]byte(rest), &d); err != nil {
		t.Fatalf("delta line is not valid JSON (%v): %s", err, rest)
	}
	return d
}

// TestFailOnNewErrors_RollsBackAndReturnsTheDelta is acceptance case (b): an
// edit that introduces a compile error with fail_on_new_errors leaves the file
// bytes unchanged and returns the structured delta.
func TestFailOnNewErrors_RollsBackAndReturnsTheDelta(t *testing.T) {
	e := newFailEnv(t, []protocol.Diagnostic{hardErr("cannot use \"x\" (untyped string) as int", 2)})

	out, err := e.edit(t, map[string]any{"fail_on_new_errors": true})
	if err == nil {
		t.Fatalf("expected the edit to be refused, got success:\n%s", out)
	}
	if got := e.content(t); got != failTestBefore {
		t.Fatalf("file must be byte-for-byte unchanged after a rollback\n got: %q\nwant: %q", got, failTestBefore)
	}
	msg := err.Error()
	if !strings.Contains(msg, "ROLLED BACK") {
		t.Errorf("refusal should say the write was rolled back, got:\n%s", msg)
	}
	if !strings.Contains(msg, "cannot use") {
		t.Errorf("refusal should list the offending error, got:\n%s", msg)
	}
	d := parseDelta(t, msg)
	if !d.Fresh || len(d.NewErrors) != 1 {
		t.Fatalf("want fresh delta with exactly 1 new error, got %+v", d)
	}
	if d.NewErrors[0].Severity != "error" || d.NewErrors[0].Line != 3 {
		t.Errorf("delta entry should carry the 1-based line and severity, got %+v", d.NewErrors[0])
	}
	if d.Scopes.EditedFile != diagScopeFresh {
		t.Errorf("edited_file scope = %q, want %q", d.Scopes.EditedFile, diagScopeFresh)
	}
	// A rolled-back write has nothing to undo — the armed snapshot must not
	// survive it pointing at content that is no longer on disk.
	if _, ok := e.undo.Peek(e.path); ok {
		t.Error("rollback must clear the undo snapshot the write armed")
	}
}

// TestFailOnNewErrors_WithoutTheFlagTheEditLands is acceptance case (c): the
// same edit, no flag — it lands, and the delta is reported rather than enforced.
func TestFailOnNewErrors_WithoutTheFlagTheEditLands(t *testing.T) {
	e := newFailEnv(t, []protocol.Diagnostic{hardErr("cannot use \"x\" (untyped string) as int", 2)})

	out, err := e.edit(t, map[string]any{"await_diagnostics": true})
	if err != nil {
		t.Fatalf("without fail_on_new_errors the edit must land: %v", err)
	}
	if got := e.content(t); got != failTestAfter {
		t.Fatalf("edit did not land\n got: %q\nwant: %q", got, failTestAfter)
	}
	d := parseDelta(t, out)
	if !d.Fresh || len(d.NewErrors) != 1 {
		t.Fatalf("want the delta reported, got %+v", d)
	}
}

// TestFailOnNewErrors_UnconfirmedNeverRollsBack is acceptance case (d): the
// analysis timed out, so the delta is not fresh — the write stays, and NOTHING
// is rolled back. Never roll back on uncertainty.
func TestFailOnNewErrors_UnconfirmedNeverRollsBack(t *testing.T) {
	e := newFailEnv(t, []protocol.Diagnostic{hardErr("cannot use \"x\" (untyped string) as int", 2)})
	e.diag.waitErr = context.DeadlineExceeded

	out, err := e.edit(t, map[string]any{"fail_on_new_errors": true})
	if err != nil {
		t.Fatalf("an unconfirmed check must not fail the write: %v", err)
	}
	if got := e.content(t); got != failTestAfter {
		t.Fatalf("the write must stand when freshness is unconfirmed\n got: %q", got)
	}
	d := parseDelta(t, out)
	if d.Fresh {
		t.Errorf("delta must report fresh:false on a timed-out check, got %+v", d)
	}
	if len(d.NewErrors) != 0 {
		t.Errorf("an unconfirmed delta must not claim findings, got %+v", d.NewErrors)
	}
	if !strings.Contains(out, postWriteDiagLabelSnapshot) {
		t.Errorf("a timed-out check must carry the snapshot label, got:\n%s", out)
	}
}

// TestFailOnNewErrors_UnfreshDeltaNeverBlocks pins the gate itself, not just the
// paths that reach it. Every unconfirmed path also happens to carry no findings,
// which would hide a missing freshness check — so this asserts the predicate
// directly on a delta that has BOTH findings and fresh:false.
func TestFailOnNewErrors_UnfreshDeltaNeverBlocks(t *testing.T) {
	d := diagDelta{
		Fresh:     false,
		NewErrors: []diagEntry{{File: "/x/a.go", Line: 1, Severity: "error", Message: "boom"}},
	}
	if d.hasNewErrors() {
		t.Fatal("a delta that is not confirmed fresh must never trigger a rollback, whatever it carries")
	}
	d.Fresh = true
	if !d.hasNewErrors() {
		t.Fatal("a confirmed delta with a new error must trigger the rollback")
	}
}

// TestFailOnNewErrors_NotifyFailureNeverRollsBack: the language server could not
// be told the file changed, so anything it reports describes the PREVIOUS
// content. Freshness must be derived from that fact, not from the caller having
// asked — and an unfresh result never rolls back.
func TestFailOnNewErrors_NotifyFailureNeverRollsBack(t *testing.T) {
	e := newFailEnv(t, []protocol.Diagnostic{hardErr("cannot use \"x\" (untyped string) as int", 2)})
	e.deps.Client = notifyFailLSP{}

	out, err := e.edit(t, map[string]any{"fail_on_new_errors": true})
	if err != nil {
		t.Fatalf("a failed notification must not fail the write: %v", err)
	}
	if got := e.content(t); got != failTestAfter {
		t.Fatalf("the write must stand when the server was never notified\n got: %q", got)
	}
	if e.diag.waitCall != 0 {
		t.Errorf("no point waiting on a server that does not know about the write; waits = %d", e.diag.waitCall)
	}
	d := parseDelta(t, out)
	if d.Fresh {
		t.Errorf("delta must report fresh:false when the notification failed, got %+v", d)
	}
	if !strings.Contains(out, postWriteDiagLabelNotAnalysed) {
		t.Errorf("expected the not-analysed label, got:\n%s", out)
	}
}

// TestFailOnNewErrors_WarningsAndPreExistingErrorsNeverBlock pins the two
// explicit "Do NOT" clauses: warnings never block, and an error that was already
// there before the write never blocks.
func TestFailOnNewErrors_WarningsAndPreExistingErrorsNeverBlock(t *testing.T) {
	standing := hardErr("undefined: elsewhere", 9)
	e := newFailEnv(t, []protocol.Diagnostic{standing, warnAt("unused variable q", 2)})
	e.diag.pre = []protocol.Diagnostic{standing}

	out, err := e.edit(t, map[string]any{"fail_on_new_errors": true})
	if err != nil {
		t.Fatalf("warnings and pre-existing errors must not roll back: %v", err)
	}
	if got := e.content(t); got != failTestAfter {
		t.Fatalf("the edit must land\n got: %q", got)
	}
	d := parseDelta(t, out)
	if len(d.NewErrors) != 0 {
		t.Errorf("no new errors expected, got %+v", d.NewErrors)
	}
	if d.PreExisting != 1 {
		t.Errorf("pre_existing = %d, want 1", d.PreExisting)
	}
}

// TestFailOnNewErrors_ResolvedDiagnosticsAreReported covers the delta's other
// half: what the edit FIXED.
func TestFailOnNewErrors_ResolvedDiagnosticsAreReported(t *testing.T) {
	e := newFailEnv(t, nil)
	e.diag.pre = []protocol.Diagnostic{hardErr("cannot use 1 (untyped int) as string", 2)}

	out, err := e.edit(t, map[string]any{"await_diagnostics": true})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	d := parseDelta(t, out)
	if len(d.Resolved) != 1 || !strings.Contains(d.Resolved[0].Message, "cannot use 1") {
		t.Fatalf("expected the fixed diagnostic in resolved, got %+v", d.Resolved)
	}
}

// TestFailOnNewErrors_ConcurrentWriterAbortsTheRollback: a peer's write landed
// between plumb's write and the analysis. Reverting would discard it, so plumb
// reports instead — the same rule undo_edit follows.
func TestFailOnNewErrors_ConcurrentWriterAbortsTheRollback(t *testing.T) {
	const peerContent = "package p // a peer wrote this\n"
	e := newFailEnv(t, []protocol.Diagnostic{hardErr("cannot use \"x\" (untyped string) as int", 2)})
	e.diag.onWait = func() {
		// Fires from inside the post-write wait, i.e. after plumb's write and
		// before the rollback decision.
		if err := os.WriteFile(e.path, []byte(peerContent), 0o644); err != nil {
			t.Errorf("simulating peer write: %v", err)
		}
	}

	out, err := e.edit(t, map[string]any{"fail_on_new_errors": true})
	if err == nil {
		t.Fatalf("expected a refusal, got success:\n%s", out)
	}
	if got := e.content(t); got != peerContent {
		t.Fatalf("the peer's content must survive the aborted rollback\n got: %q", got)
	}
	if !strings.Contains(err.Error(), "could NOT be rolled back") {
		t.Errorf("the refusal must say the rollback did not happen, got:\n%s", err.Error())
	}
}

// TestFailOnNewErrors_DecisionHappensUnderThePathLock proves the write, the
// analysis and the rollback decision are ONE critical section, by probing the
// lock from inside the analysis callback rather than racing a sleep against it.
func TestFailOnNewErrors_DecisionHappensUnderThePathLock(t *testing.T) {
	e := newFailEnv(t, []protocol.Diagnostic{hardErr("cannot use \"x\" (untyped string) as int", 2)})

	var heldDuringAnalysis, competitorEntered bool
	competitorDone := make(chan struct{})
	e.diag.onWait = func() {
		heldDuringAnalysis = !tryPathLock(e.path)
		// The probe must be able to say "not held" too, or "held" proves nothing.
		if !tryPathLock(e.path + ".unrelated") {
			t.Error("probe is vacuous: it reports an untouched path as locked")
		}
		// A second writer must not be able to enter while we decide.
		go func() {
			defer close(competitorDone)
			unlock := lockPath(e.path)
			competitorEntered = true
			unlock()
		}()
		time.Sleep(20 * time.Millisecond)
		if competitorEntered {
			t.Error("a competing writer acquired the path lock while the rollback decision was pending")
		}
	}

	if _, err := e.edit(t, map[string]any{"fail_on_new_errors": true}); err == nil {
		t.Fatal("expected the refusal")
	}
	<-competitorDone
	if !heldDuringAnalysis {
		t.Error("the per-path lock was NOT held while post-write diagnostics were analysed")
	}
}

// tryPathLock reports whether the path lock could be acquired right now,
// releasing it again if so. Used only as a probe from inside a callback the
// locked region invokes.
func tryPathLock(path string) bool {
	v, ok := pathLocks.Load(lockPathKey(path))
	if !ok {
		return true // no entry at all: certainly not held
	}
	e := v.(*pathLockEntry)
	if !e.mu.TryLock() {
		return false
	}
	e.mu.Unlock()
	return true
}

// TestFailOnNewErrors_RefusedOverTheSnapshotCap: a file too large to snapshot is
// refused UP-FRONT, before anything is written — discovering the missing safety
// net after the broken content is on disk would be the worst of both worlds.
func TestFailOnNewErrors_RefusedOverTheSnapshotCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.go")
	big := strings.Repeat("x", maxUndoSnapshotBytes+1)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := NewEditFile(WriteDeps{Reads: NewReadTracker()}).Execute(context.Background(), mustJSON(map[string]any{
		"file_path":          path,
		"fail_on_new_errors": true,
		"edits":              []map[string]any{{"old_string": "xxx", "new_string": "yyy", "replace_all": true}},
	}))
	if err == nil {
		t.Fatal("expected fail_on_new_errors to be refused on an over-cap file")
	}
	if !strings.Contains(err.Error(), "snapshot cap") {
		t.Errorf("the refusal should name the cap, got: %v", err)
	}
	b, readErr := os.ReadFile(path)
	if readErr != nil || string(b) != big {
		t.Error("an up-front refusal must not have touched the file")
	}
}

// TestFailOnNewErrors_RefusedWithApplyPartial: apply_partial deliberately lets
// some edits land and others fail, which is the opposite of the all-or-nothing
// promise. The combination is refused rather than half-honoured.
func TestFailOnNewErrors_RefusedWithApplyPartial(t *testing.T) {
	e := newFailEnv(t, nil)
	_, err := e.edit(t, map[string]any{"fail_on_new_errors": true, "apply_partial": true})
	if err == nil || !strings.Contains(err.Error(), "apply_partial") {
		t.Fatalf("expected the combination to be refused, got: %v", err)
	}
	if got := e.content(t); got != failTestBefore {
		t.Errorf("a refused call must not write, got %q", got)
	}
}

// TestFailOnNewErrors_WriteFileRemovesAFileItCreated: for write_file the
// "pre-write content" of a new file is its absence, so the rollback deletes it.
func TestFailOnNewErrors_WriteFileRemovesAFileItCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.go")
	d := &scriptedDiag{post: []protocol.Diagnostic{hardErr("expected declaration", 0)}}
	deps := WriteDeps{Diag: d, Undo: NewUndoStore(), Reads: NewReadTracker(), Writes: NewWriteTracker()}

	_, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path":          path,
		"content":            "package\n",
		"fail_on_new_errors": true,
	}))
	if err == nil {
		t.Fatal("expected the write to be refused")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("a rolled-back creation must leave no file behind (stat err: %v)", statErr)
	}
}

// TestFailOnNewErrors_WriteFileRefusesWhenItCannotSnapshot: if the pre-write
// bytes cannot be captured, a "rollback" would restore an empty file — worse
// than the breakage it prevents. The write is refused before it happens.
func TestFailOnNewErrors_WriteFileRefusesWhenItCannotSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unreadable.go")
	if err := os.WriteFile(path, []byte(failTestBefore), 0o200); err != nil { // write-only
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this filesystem/user can read a 0200 file; the unreadable case cannot be simulated here")
	}

	_, err := NewWriteFile(WriteDeps{Diag: &scriptedDiag{}, Reads: NewReadTracker()}).
		Execute(context.Background(), mustJSON(map[string]any{
			"file_path":          path,
			"content":            failTestAfter,
			"fail_on_new_errors": true,
		}))
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("expected a refusal naming the missing snapshot, got: %v", err)
	}
	_ = os.Chmod(path, 0o644)
	if got := readFileString(t, path); got != failTestBefore {
		t.Errorf("a refused write must not have touched the file, got %q", got)
	}
}

// TestFailOnNewErrors_WriteFileRestoresPreviousContent covers the overwrite case
// end-to-end on the bytes.
func TestFailOnNewErrors_WriteFileRestoresPreviousContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(failTestBefore), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := &scriptedDiag{post: []protocol.Diagnostic{hardErr("cannot use \"x\" (untyped string) as int", 2)}}
	deps := WriteDeps{Diag: d, Undo: NewUndoStore(), Reads: NewReadTracker(), Writes: NewWriteTracker()}

	_, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path":          path,
		"content":            failTestAfter,
		"fail_on_new_errors": true,
	}))
	if err == nil {
		t.Fatal("expected the write to be refused")
	}
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if string(b) != failTestBefore {
		t.Fatalf("file must be byte-for-byte unchanged\n got: %q\nwant: %q", b, failTestBefore)
	}
}
