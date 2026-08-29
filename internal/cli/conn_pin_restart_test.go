package cli

// conn_pin_restart_test.go — the promise session_start's reconnect note makes:
// "The daemon restores an explicit session_start workspace."
//
// These exist because that promise was REPORTED broken and was not. A session
// pinned to project A came back on project B across a daemon upgrade, and the
// restore path was blamed; the daemon log showed the restore had replayed A
// byte-for-byte and a co-tenant agent on the same connection had force-re-pinned
// to B eight minutes later (see conn_pin_contest.go for what came of that).
//
// The distinction is worth a test of its own rather than a note in a review.
// "The pin came back wrong" and "the pin was taken afterwards" call for opposite
// fixes, and the only thing that told them apart was a log line that could have
// been rotated away. TestExplicitRepinSurvivesDaemonRestart
// (conn_attachoninit_test.go) already covers the ordering that makes the pin
// outrank client roots; what is asserted here is the narrower, absolute
// property the report actually disputed: the restored root is the STORED
// STRING, and a connection with nothing to restore comes back attached to
// NOTHING rather than to something else.

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// rootsSilent answers roots/list the way a client with no roots capability
// does: nothing to report. It is the shape that makes "unpinned" observable —
// with a roots answer available the ladder would attach from it and the
// fail-closed rung would never be reached.
func rootsSilent() mcp.RequestFn {
	return func(_ context.Context, _ string, _ any) (json.RawMessage, error) {
		return json.RawMessage(`{"roots":[]}`), nil
	}
}

// TestPin_SurvivesDaemonRestartByteIdentical: the workspace a caller declared
// with session_start comes back across a daemon restart as the same string, not
// as something that merely resolves near it.
//
// Byte-identity is the assertion, not "is inside A" or "detects to A": every
// silent cross-repository write in this area came from a root that resolved to
// an ANCESTOR or an alias of the one the caller chose, each of which would pass
// a containment check while widening what the session may write.
func TestPin_SurvivesDaemonRestartByteIdentical(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	before := newPersistSession(t, store, ss, "proxyX")
	pinned, err := before.repinWorkspace(context.Background(), root, "", false)
	if err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	// The restart: a fresh connSession, same proxy session ID, same store, and a
	// client that reports no roots at all.
	after := newPersistSession(t, store, ss, "proxyX")
	after.attachOnInit(context.Background(), rootsSilent())

	if got := after.workspace(); got != pinned {
		t.Fatalf("after restart the connection is on %q; the declared pin %q must come back byte-identical", got, pinned)
	}
	// The provenance must say it was restored, not freshly declared: an agent
	// re-orienting after a restart reads this to tell "I chose this" from "this
	// was replayed for me".
	if prov := after.pinProvenance(); prov.Source != "restore:session_start" {
		t.Errorf("restored pin provenance = %q, want restore:session_start", prov.Source)
	}
	// And a restore is not a displacement: nothing was overridden, so the pin
	// must not carry the mark that tells a reader someone took it.
	if prov := after.pinProvenance(); prov.Forced || prov.Previous != "" {
		t.Errorf("a restored pin looks like a forced displacement: %+v", prov)
	}
}

// TestPin_UnsetComesBackUnattachedNotElsewhere is the fail-closed half, and the
// one that makes the test above mean anything. A connection with no pin to
// restore and no roots to attach from must come back attached to NOTHING.
//
// Unattached is recoverable — the next path-bearing call is refused with
// UnattachedWorkspaceError, which names the fix. Attached to the wrong project
// is not: a relative-path write lands in another repository and nothing refuses
// it. The report this file answers claimed exactly that outcome, so the absence
// of a fallback that could produce it is worth pinning.
func TestPin_UnsetComesBackUnattachedNotElsewhere(t *testing.T) {
	store, ss := newOriginStore(t)
	other := freshTempDir(t) // a project on disk that nothing may drift onto
	mustGitDir(t, other)

	s := newPersistSession(t, store, ss, "proxyFresh")
	s.attachOnInit(context.Background(), rootsSilent())

	if got := s.workspace(); got != "" {
		t.Fatalf("a connection with no pin and no roots attached to %q; it must come back unattached", got)
	}
	if got := s.workspace(); got == other {
		t.Fatalf("the connection drifted onto an unrelated project %q", other)
	}
}

// TestPin_RestoreDoesNotResolveAfresh: the restore replays the STORED path. A
// marker appearing above the pinned root between the pin and the restart must
// not silently widen the session to that ancestor — the shape of the #181
// fail-open, and the one the report's hypothesis described.
func TestPin_RestoreDoesNotResolveAfresh(t *testing.T) {
	store, ss := newOriginStore(t)
	parent := freshTempDir(t)
	child := parent + "/nested"
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitDir(t, child)

	before := newPersistSession(t, store, ss, "proxyX")
	pinned, err := before.repinWorkspace(context.Background(), child, "", false)
	if err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	// A .git appears ABOVE the pinned root while the daemon is down.
	mustGitDir(t, parent)

	after := newPersistSession(t, store, ss, "proxyX")
	after.attachOnInit(context.Background(), rootsSilent())

	switch got := after.workspace(); got {
	case pinned:
		// Restored exactly. Correct.
	case "":
		// Refused rather than widened. Also correct, and fail-closed.
		t.Log("restore declined the pin rather than replaying it; unattached is the safe outcome")
	case parent:
		t.Fatalf("the restore widened the session to the ancestor %q; the pinned root was %q", parent, pinned)
	default:
		t.Fatalf("restore landed on %q, which is neither the pinned root %q nor unattached", got, pinned)
	}
}
