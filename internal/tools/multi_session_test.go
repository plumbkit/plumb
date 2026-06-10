package tools

// multi_session_test.go — the in-process tier of the multi-agent coexistence
// tests (review-plan B4 W1). Each newSessionDeps call stands in for one MCP
// connection's per-session write dependencies; the per-path write locks
// (pathLocks, file_write_helpers.go) are process-global by design, so two
// sessions in one test process exercise the same serialisation contract as two
// daemon connections. Tests assert invariants (content, refusals, rollback),
// never interleavings, and advance mtimes with os.Chtimes rather than sleeps.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newSessionDeps builds the fully-wired per-session write dependencies a
// daemon connection would get from handleConn: its own ReadTracker,
// WriteTracker, and rate limiter (per-session isolation), with the workspace
// pinned so transaction_apply can reach the txlog.
func newSessionDeps(t *testing.T, ws string) WriteDeps {
	t.Helper()
	return WriteDeps{
		Reads:       NewReadTracker(),
		Writes:      NewWriteTracker(),
		Limiter:     NewRateLimiter(10000, time.Minute),
		WorkspaceFn: func() string { return ws },
	}
}

func editFileAs(deps WriteDeps, path, oldStr, newStr string, extra map[string]any) (string, error) {
	args := map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": oldStr, "new_string": newStr}},
	}
	for k, v := range extra {
		args[k] = v
	}
	return NewEditFile(deps).Execute(context.Background(), mustJSON(args))
}

// nudgeMtimeForward advances path's mtime well past any recorded read/write
// time, simulating the clock having moved on after a peer's edit without
// sleeping.
func nudgeMtimeForward(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// TestMultiSession_ConcurrentEditSamePath_Serialised: two sessions edit the
// same file concurrently. The process-global per-path lock must serialise the
// writes so both edits land and each session's marker appears exactly once.
func TestMultiSession_ConcurrentEditSamePath_Serialised(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "shared.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionA := newSessionDeps(t, ws)
	sessionB := newSessionDeps(t, ws)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = editFileAs(sessionA, path, "alpha", "alpha [marker-A]", nil)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = editFileAs(sessionB, path, "beta", "beta [marker-B]", nil)
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d edit failed: %v", i, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"[marker-A]", "[marker-B]"} {
		if n := strings.Count(string(got), marker); n != 1 {
			t.Errorf("marker %s appears %d times, want exactly 1; content:\n%s", marker, n, got)
		}
	}
}

// TestMultiSession_StaleExpectedMtime_Rejected: session A captures the file's
// version (mtime and sha), session B edits it, and A's guarded edits must be
// refused on both the mtime and the sha guard.
func TestMultiSession_StaleExpectedMtime_Rejected(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "guarded.txt")
	if err := os.WriteFile(path, []byte("v1 content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionA := newSessionDeps(t, ws)
	sessionB := newSessionDeps(t, ws)

	// A captures the version guards it would have taken from a read_file header.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	staleMtime := info.ModTime().Format(time.RFC3339Nano)
	staleSha, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}

	// B edits the file; the nudge guarantees the on-disk mtime differs from A's
	// capture even on coarse-grained filesystems.
	if _, err := editFileAs(sessionB, path, "v1 content", "v2 content (peer)", nil); err != nil {
		t.Fatalf("peer edit failed: %v", err)
	}
	nudgeMtimeForward(t, path)

	_, err = editFileAs(sessionA, path, "v2 content (peer)", "v3 by A", map[string]any{"expected_mtime": staleMtime})
	if err == nil || !strings.Contains(err.Error(), "was modified since you read it") {
		t.Fatalf("stale expected_mtime must be rejected, got: %v", err)
	}

	_, err = editFileAs(sessionA, path, "v2 content (peer)", "v3 by A", map[string]any{"expected_sha": staleSha})
	if err == nil || !strings.Contains(err.Error(), "content has changed since you read it") {
		t.Fatalf("stale expected_sha must be rejected, got: %v", err)
	}

	// The refused edits must not have touched the file.
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "v2 content (peer)") || strings.Contains(string(got), "v3 by A") {
		t.Fatalf("refused edits must leave the peer's content intact, got: %q", got)
	}
}

// TestMultiSession_PeerWrite_TriggersStaleSessionReadGuard: session A read the
// file, a peer session then edited it, and A issues unguarded writes. The
// automatic session-aware guard must refuse write_file's full overwrite and
// warn on edit_file.
func TestMultiSession_PeerWrite_TriggersStaleSessionReadGuard(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "watched.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionA := newSessionDeps(t, ws)
	sessionB := newSessionDeps(t, ws)

	// A reads the file (recorded in A's per-session ReadTracker only).
	if _, err := NewReadFile(sessionA.Reads).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	// B edits it; the nudge puts the mtime firmly past A's recorded read.
	if _, err := editFileAs(sessionB, path, "two", "two (peer)", nil); err != nil {
		t.Fatalf("peer edit failed: %v", err)
	}
	nudgeMtimeForward(t, path)

	// A's unguarded full overwrite is refused (it would discard B's edit).
	_, err := NewWriteFile(sessionA).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path, "content": "A's full rewrite\n",
	}))
	if err == nil || !strings.Contains(err.Error(), "changed on disk since you read it this session") {
		t.Fatalf("unguarded overwrite after a peer write must be refused, got: %v", err)
	}

	// A's unguarded edit still applies (the anchor protects the edited region)
	// but must carry the concurrent-change warning.
	out, err := editFileAs(sessionA, path, "one", "one (A)", nil)
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	if !strings.Contains(out, "plumb-warn") {
		t.Fatalf("edit after a peer write must warn, got:\n%s", out)
	}
}

// TestMultiSession_TransactionRollback_OnPeerConflict: session A prepares a
// two-file transaction guarded by a sha it captured before session B edited
// one of the files. The transaction must reject all-or-nothing: the
// conflicting file keeps the peer's content, the other file is untouched, and
// no txlog snapshot is left behind.
func TestMultiSession_TransactionRollback_OnPeerConflict(t *testing.T) {
	ws := initPlumbWorkspace(t)
	a := filepath.Join(ws, "a.txt")
	b := filepath.Join(ws, "b.txt")
	if err := os.WriteFile(a, []byte("original-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("original-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionA := newSessionDeps(t, ws)
	sessionB := newSessionDeps(t, ws)

	// A captures b.txt's version for its transaction.
	staleSha, err := fileSHA256(b)
	if err != nil {
		t.Fatal(err)
	}

	// B edits b.txt between A's capture and A's transaction.
	if _, err := editFileAs(sessionB, b, "original-b", "peer-b", nil); err != nil {
		t.Fatalf("peer edit failed: %v", err)
	}

	_, err = NewTransactionApply(sessionA).Execute(context.Background(), mustJSON(map[string]any{
		"operations": []map[string]any{
			{"file_path": a, "edits": []map[string]string{{"old_string": "original-a", "new_string": "tx-a"}}},
			{"file_path": b, "expected_sha": staleSha, "edits": []map[string]string{{"old_string": "peer-b", "new_string": "tx-b"}}},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "content has changed") {
		t.Fatalf("transaction with a peer-conflicted sha must be rejected, got: %v", err)
	}

	if got, _ := os.ReadFile(a); string(got) != "original-a" {
		t.Errorf("a.txt must be untouched after the rejected transaction, got: %q", got)
	}
	if got, _ := os.ReadFile(b); string(got) != "peer-b" {
		t.Errorf("b.txt must keep the peer's content, got: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(ws, ".plumb", "tx-log"))
	if len(entries) > 0 {
		t.Errorf("no txlog snapshot may remain after a rejected transaction, found: %v", entries)
	}
}

// TestMultiSession_RaceWriteSamePath_NoCorruption: many writers across two
// sessions race full-file writes to one path. The per-path lock plus atomic
// tmp-and-rename must leave the file equal to exactly one writer's payload,
// never a torn mix. The writers pass overwrite_changed so the session-aware
// staleness guard (covered by the PeerWrite test above) does not refuse the
// later writers; this test isolates the serialisation and atomicity contract.
func TestMultiSession_RaceWriteSamePath_NoCorruption(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "raced.txt")

	sessions := []WriteDeps{newSessionDeps(t, ws), newSessionDeps(t, ws)}

	const writers = 8
	payloads := make([]string, writers)
	for i := range payloads {
		// Large distinct payloads so a torn or interleaved write is detectable.
		payloads[i] = strings.Repeat(fmt.Sprintf("writer-%d line\n", i), 4096)
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			deps := sessions[i%len(sessions)]
			_, errs[i] = NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
				"file_path": path, "content": payloads[i], "overwrite_changed": true,
			}))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", i, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range payloads {
		if string(got) == p {
			return // exactly one writer's payload, intact
		}
	}
	t.Fatalf("final content matches no single writer's payload (len=%d); torn write?", len(got))
}
