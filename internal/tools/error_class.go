package tools

// error_class.go — the tools package's refusal seams, classified.
//
// Each helper here attaches a machine-readable classification
// (internal/toolerror) to an error the call site has already worded. The
// wording is never touched: toolerror.Error.Error() returns the wrapped cause
// verbatim, so a client that reads the sentence sees exactly what it saw
// before, and one that reads the classification learns which of plumb's
// refusals it hit and what to do about it.
//
// Several of these compose with *editLogicErr. The order matters only in that
// the toolerror wrapper goes OUTSIDE, so errors.As finds both: the
// classification via toolerror.Classify, and the retry verdict via
// isEditLogicError, exactly as before.

import (
	"github.com/plumbkit/plumb/internal/toolerror"
)

// staleRead classifies a read-before-write or optimistic-concurrency refusal —
// the caller never read the file, or read a version that has since changed.
// Retryable, because re-reading makes the same intent succeed; NOT replayable,
// because replaying the identical write is the clobber the guard prevents.
func staleRead(err error) error {
	return toolerror.Wrap(err, toolerror.KindUnreadOrStale, toolerror.ClassReRead,
		toolerror.WithTool("read_file"), toolerror.Retry())
}

// dirtyWrite classifies the uncommitted-changes guard on a write.
func dirtyWrite(err error) error {
	return toolerror.Wrap(err, toolerror.KindDirtyFile, toolerror.ClassPassDirtyOk,
		toolerror.Retry())
}

// lspTimedOut classifies a language server that did not answer within the
// operation's deadline. Retryable: a server that is indexing answers the same
// question once it is done.
func lspTimedOut(err error) error {
	return toolerror.Wrap(err, toolerror.KindLSPTimeout, toolerror.ClassRetryWhenReady,
		toolerror.Retry())
}

// lspNotReady classifies a hard failure against a still-warming language
// server — distinct from a timeout in that plumb never asked: it knows the
// server cannot answer yet.
func lspNotReady(err error) error {
	return toolerror.Wrap(err, toolerror.KindLSPUnavailable, toolerror.ClassRetryWhenReady,
		toolerror.Retry())
}

// ClassifyPathRefusal attaches the workspace-boundary classification to a
// WorkspaceBoundaryError or UnattachedWorkspaceError. It is exported because
// the daemon's BoundaryGuard (internal/cli) constructs the unattached refusal
// itself, and a refusal classified in three of four places is worse than one
// classified nowhere — a client would learn to trust a signal that is
// sometimes absent.
//
// Not retryable: the same call against the same connection will be refused
// again for as long as the pin stands. The remedy is a different pin, not a
// wait, so the remediation names session_start rather than a delay.
func ClassifyPathRefusal(err error) error {
	return toolerror.New(toolerror.KindWorkspaceBoundary, err, toolerror.Remediation{
		Class: toolerror.ClassRepinWorkspace,
		Tool:  "session_start",
		Reason: "This connection is not pinned to the project the path belongs to; " +
			"re-pin it with session_start, then retry.",
	})
}
