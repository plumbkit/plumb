package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// assertClassified checks the classification WITHOUT letting the message drift:
// every case pins the rendered text against the same error unwrapped, so a
// future edit to a refusal sentence fails here only if the wrapper changed it.
func assertClassified(t *testing.T, err error, wantKind toolerror.Kind, wantClass toolerror.RemediationClass, wantRetry bool) {
	t.Helper()
	te := mustClassify(t, err)
	if te.Kind != wantKind {
		t.Errorf("Kind = %q, want %q", te.Kind, wantKind)
	}
	if te.Remediation.Class != wantClass {
		t.Errorf("Remediation.Class = %q, want %q", te.Remediation.Class, wantClass)
	}
	if te.Retryable() != wantRetry {
		t.Errorf("Retryable = %v, want %v", te.Retryable(), wantRetry)
	}
	if te.Remediation.Reason == "" {
		t.Error("Remediation.Reason is empty; every class must carry a sentence")
	}
}

// mustClassify returns the classification carried by err, failing the test when
// there is none.
func mustClassify(t *testing.T, err error) *toolerror.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	te, ok := toolerror.Classify(err)
	if !ok {
		t.Fatalf("error is not classified: %v", err)
	}
	return te
}

func TestClassificationHelpers_PreserveText(t *testing.T) {
	cause := errors.New("some_tool: a carefully worded refusal\n  with a second line")
	tests := []struct {
		name  string
		wrap  func(error) error
		kind  toolerror.Kind
		class toolerror.RemediationClass
		retry bool
		tool  string
	}{
		{"staleRead", staleRead, toolerror.KindUnreadOrStale, toolerror.ClassReRead, true, "read_file"},
		{"dirtyWrite", dirtyWrite, toolerror.KindDirtyFile, toolerror.ClassPassDirtyOk, true, ""},
		{"staleOverride", staleOverride, toolerror.KindUnreadOrStale, toolerror.ClassPassForce, true, ""},
		{"badArgument", badArgument, toolerror.KindInvalidArguments, toolerror.ClassFixArguments, true, ""},
		{"lspTimedOut", lspTimedOut, toolerror.KindLSPTimeout, toolerror.ClassRetryWhenReady, true, ""},
		{"lspNotReady", lspNotReady, toolerror.KindLSPUnavailable, toolerror.ClassRetryWhenReady, true, ""},
		{
			// repin_workspace is retryable: the agent itself calls session_start.
			"ClassifyPathRefusal", ClassifyPathRefusal,
			toolerror.KindWorkspaceBoundary, toolerror.ClassRepinWorkspace, true, "session_start",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.wrap(cause)
			assertClassified(t, err, tt.kind, tt.class, tt.retry)
			if te := mustClassify(t, err); te.Remediation.Tool != tt.tool {
				t.Errorf("Remediation.Tool = %q, want %q", te.Remediation.Tool, tt.tool)
			}
			if err.Error() != cause.Error() {
				t.Errorf("text = %q, want the cause verbatim %q", err.Error(), cause.Error())
			}
			if !errors.Is(err, cause) {
				t.Error("errors.Is lost the cause through the classification")
			}
		})
	}
}

// TestClassificationComposesWithEditLogicErr pins the composition contract: a
// site that already marked its refusal "do not retry this verbatim" must keep
// that marker AND gain a classification. Losing either would silently change
// edit_file's retry behaviour or leave the seam unclassified.
func TestClassificationComposesWithEditLogicErr(t *testing.T) {
	inner := &editLogicErr{errors.New("edit_file: %q has uncommitted changes")}
	err := dirtyWrite(inner)

	if !isEditLogicError(err) {
		t.Error("isEditLogicError no longer sees through the classification")
	}
	assertClassified(t, err, toolerror.KindDirtyFile, toolerror.ClassPassDirtyOk, true)
	if err.Error() != inner.Error() {
		t.Errorf("text = %q, want %q", err.Error(), inner.Error())
	}
}

func TestRateLimitError_Classified(t *testing.T) {
	lim := NewRateLimiter(1, time.Minute)
	if !lim.Allow() {
		t.Fatal("first Allow should succeed")
	}
	err := rateLimitError("write_file", lim)
	assertClassified(t, err, toolerror.KindRateLimited, toolerror.ClassRetryAfterWait, true)
	if !isEditLogicError(err) {
		t.Error("rate-limit refusal lost its editLogicErr marker")
	}
	if want := "write_file: rate limit exceeded"; len(err.Error()) < len(want) || err.Error()[:len(want)] != want {
		t.Errorf("text = %q, want it to start with %q", err.Error(), want)
	}
}

func TestRequireStrictRead_Classified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := NewReadTracker()

	t.Run("never read", func(t *testing.T) {
		err := requireStrictRead(reads, "edit_file", path)
		assertClassified(t, err, toolerror.KindUnreadOrStale, toolerror.ClassReRead, true)
		if te := mustClassify(t, err); te.Remediation.Tool != "read_file" {
			t.Errorf("Remediation.Tool = %q, want read_file", te.Remediation.Tool)
		}
	})

	t.Run("changed since read", func(t *testing.T) {
		reads.Record(path, time.Now().Add(-time.Hour), "")
		err := requireStrictRead(reads, "edit_file", path)
		assertClassified(t, err, toolerror.KindUnreadOrStale, toolerror.ClassReRead, true)
	})
}

func TestVerifyExpectedVersion_Classified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	stale := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	err := verifyExpectedVersion("write_file", path, stale, "")
	assertClassified(t, err, toolerror.KindUnreadOrStale, toolerror.ClassReRead, true)

	err = verifyExpectedVersion("write_file", path, "", "0000000000000000000000000000000000000000000000000000000000000000")
	assertClassified(t, err, toolerror.KindUnreadOrStale, toolerror.ClassReRead, true)

	// A malformed expected_mtime is the caller's argument, not a concurrent
	// change — so it is classified, but as invalid_arguments, never as a
	// staleness refusal that would send the caller to re-read a fine file.
	err = verifyExpectedVersion("write_file", path, "not-a-time", "")
	assertClassified(t, err, toolerror.KindInvalidArguments, toolerror.ClassFixArguments, true)
}

// TestOverwriteChangedGuard_Classified covers the OTHER stale-write path in
// write_file: it sits three lines below the classified expected_mtime guard, so
// leaving it bare meant one tool emitted _meta for one stale write and nothing
// for the other.
func TestOverwriteChangedGuard_Classified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := NewReadTracker()
	// Recorded as read with an old mtime and a SHA that no longer matches, so
	// changedSinceSessionRead fires.
	reads.Record(path, time.Now().Add(-time.Hour), "stale-sha")

	tool := &WriteFile{deps: WriteDeps{Reads: reads}}
	err := tool.writeFilePreconditions(t.Context(), path, writeFileArgs{DirtyOk: true, Content: "new"})
	assertClassified(t, err, toolerror.KindUnreadOrStale, toolerror.ClassPassForce, true)
	if !strings.Contains(err.Error(), "overwrite_changed: true") {
		t.Errorf("message lost the flag it advertises: %q", err.Error())
	}
}

// TestUndoEditForceGuards_Classified covers undo_edit's two "pass force:true"
// refusals. They are one seam with one recovery, so classifying only the
// changed-under-you branch would leave the deleted-file branch silent for the
// identical remedy.
func TestUndoEditForceGuards_Classified(t *testing.T) {
	dir := t.TempDir()
	tool := &UndoEdit{}

	t.Run("file changed since plumb wrote it", func(t *testing.T) {
		path := filepath.Join(dir, "changed.txt")
		if err := os.WriteFile(path, []byte("edited by a peer"), 0o600); err != nil {
			t.Fatal(err)
		}
		snap := undoSnapshot{before: "old", existedBefore: true, afterSHA: "a-different-hash", tool: "edit_file"}
		err := tool.checkUndoSafe(path, snap, false)
		assertClassified(t, err, toolerror.KindUnreadOrStale, toolerror.ClassPassForce, true)
	})

	t.Run("file no longer exists", func(t *testing.T) {
		err := tool.checkUndoSafe(filepath.Join(dir, "gone.txt"), undoSnapshot{existedBefore: true}, false)
		assertClassified(t, err, toolerror.KindUnreadOrStale, toolerror.ClassPassForce, true)
	})

	t.Run("force skips the guard entirely", func(t *testing.T) {
		if err := tool.checkUndoSafe(filepath.Join(dir, "gone.txt"), undoSnapshot{}, true); err != nil {
			t.Errorf("force must skip the check, got %v", err)
		}
	})
}

// TestBoundaryRefusalKeepsItsContract pins the two behaviours the boundary doc
// comment forbids breaking: IsWorkspaceBoundaryError stays errors.As-based and
// still fires, and the message is untouched.
func TestBoundaryRefusalKeepsItsContract(t *testing.T) {
	raw := WorkspaceBoundaryError{Workspace: "/ws", Path: "/elsewhere/f.go"}
	err := ClassifyPathRefusal(raw)

	if !IsWorkspaceBoundaryError(err) {
		t.Error("IsWorkspaceBoundaryError no longer recognises a classified boundary refusal")
	}
	if err.Error() != raw.Error() {
		t.Errorf("text = %q, want %q", err.Error(), raw.Error())
	}
	var recovered WorkspaceBoundaryError
	if !errors.As(err, &recovered) || recovered.Path != "/elsewhere/f.go" {
		t.Errorf("errors.As could not recover the typed refusal: %+v", recovered)
	}

	// It crosses a package boundary (internal/cli calls it), so a nil in must
	// stay a nil out rather than becoming an error with no text.
	if got := ClassifyPathRefusal(nil); got != nil {
		t.Errorf("ClassifyPathRefusal(nil) = %v (text %q), want nil", got, got.Error())
	}

	unattached := ClassifyPathRefusal(UnattachedWorkspaceError{Path: "/p"})
	if !IsWorkspaceBoundaryError(unattached) {
		t.Error("IsWorkspaceBoundaryError no longer recognises a classified unattached refusal")
	}
	assertClassified(t, unattached, toolerror.KindWorkspaceBoundary, toolerror.ClassRepinWorkspace, true)
}

func TestLSPTimeoutSeams_Classified(t *testing.T) {
	deadline := fmt.Errorf("call: %w", context.DeadlineExceeded)

	assertClassified(t, lspTimeoutErr("find_references", 5*time.Second, deadline),
		toolerror.KindLSPTimeout, toolerror.ClassRetryWhenReady, true)
	assertClassified(t, lspTimeout("find_references", deadline),
		toolerror.KindLSPTimeout, toolerror.ClassRetryWhenReady, true)
	// positionErr and resolvedSymbolErr route a deadline through lspTimeout, so
	// the classification must survive the indirection.
	assertClassified(t, positionErr("get_definition", deadline),
		toolerror.KindLSPTimeout, toolerror.ClassRetryWhenReady, true)
	assertClassified(t, resolvedSymbolErr("get_definition", "Foo", deadline),
		toolerror.KindLSPTimeout, toolerror.ClassRetryWhenReady, true)

	// A non-deadline LSP failure keeps the coordinate hint and stays unclassified.
	if _, ok := toolerror.Classify(positionErr("get_definition", errors.New("boom"))); ok {
		t.Error("an ordinary LSP failure was classified as a timeout")
	}
	if lspTimeout("get_definition", errors.New("boom")) != nil {
		t.Error("lspTimeout fired for a non-deadline error")
	}
}
