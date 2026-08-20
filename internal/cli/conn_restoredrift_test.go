package cli

// conn_restoredrift_test.go — issue #347: a restore that re-resolves to a
// different root than restoreRootIntact already verified must be refused, not
// attached.
//
// restoreRootIntact answers "does this root still resolve to itself?" and
// hands the caller only a bool — every restore-path caller then re-runs
// pool.Detect or pool.SynthesiseRoot on the verified root anyway, to recover
// the language or the synthetic split restoreRootIntact does not return. That
// second call is a fresh, uncached filesystem walk: a marker added or removed
// above the root in the interval between the two can make it land somewhere
// else. undeclaredWideRootErr does not catch that drift on the restore path,
// because it exempts PinSourceSessionStart outright — the origin every
// restore carries — so nothing else stood between the second answer and
// attach. These tests pin the contract: under pinTriggerRestore, the attached
// root is the root that was verified, or nothing.
//
// Each test below (except the synthetic-branch one, which explains why it
// cannot) was mutation-checked: removing the matching restoreDriftErr call
// makes it fail.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

// TestRepinRestore_RefusesDriftToAncestor covers the guard in
// repinWorkspaceFrom (conn_repin.go): sub is a plain, markerless subdirectory
// of the git-rooted parent. A restore-triggered repin of sub must not be
// allowed to re-Detect its way up to parent — the caller asked to restore sub,
// not parent.
func TestRepinRestore_RefusesDriftToAncestor(t *testing.T) {
	store, ss := newOriginStore(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	sub := filepath.Join(parent, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspaceFrom(context.Background(), sub, "", sessionstate.PinSourceSessionStart, pinTriggerRestore, false); err == nil {
		t.Fatal("repinWorkspaceFrom under restore must refuse a re-Detect that lands on a different root than the one requested")
	}
	if got := s.workspace(); got == parent {
		t.Fatalf("FAIL-OPEN: restore drifted to the enclosing root %q", got)
	}
	if got := s.workspace(); got != "" {
		t.Fatalf("refused drift still left the session attached to %q", got)
	}
}

// TestRepinRestore_SameRootStillAttaches is the non-regression twin: when sub
// has its own marker, Detect lands back on sub itself and the restore must
// still succeed — rung 1/1b's ordinary case.
func TestRepinRestore_SameRootStillAttaches(t *testing.T) {
	store, ss := newOriginStore(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	sub := filepath.Join(parent, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitDir(t, sub)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	root, err := s.repinWorkspaceFrom(context.Background(), sub, "", sessionstate.PinSourceSessionStart, pinTriggerRestore, false)
	if err != nil {
		t.Fatalf("repinWorkspaceFrom: %v", err)
	}
	if root != sub || s.workspace() != sub {
		t.Fatalf("a root that resolves to itself must still restore: root=%q workspace=%q, want %q", root, s.workspace(), sub)
	}
}

// TestRepinLive_StillResolvesSubdirToRoot proves the guard is restore-only:
// onRootsChanged and a live session_start still resolve a markerless
// subdirectory up to its enclosing root exactly as before — that resolution
// is the ordinary behaviour of a fresh pin, not drift.
func TestRepinLive_StillResolvesSubdirToRoot(t *testing.T) {
	store, ss := newOriginStore(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	sub := filepath.Join(parent, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	root, err := s.repinWorkspaceFrom(context.Background(), sub, "", sessionstate.PinSourceSessionStart, pinTriggerLive, false)
	if err != nil {
		t.Fatalf("a live re-pin of a markerless subdirectory must still resolve to its enclosing root: %v", err)
	}
	if root != parent || s.workspace() != parent {
		t.Fatalf("live repin: root=%q workspace=%q, want %q", root, s.workspace(), parent)
	}
}

// TestAttachWorkspacePinFrom_RestoreDoesNotWiden covers the guard in
// attachWorkspacePinFrom (conn_attach.go): the same sub/parent shape, driven
// through the attach path rehydratePin's non-synthetic branch actually calls.
func TestAttachWorkspacePinFrom_RestoreDoesNotWiden(t *testing.T) {
	store, ss := newOriginStore(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	sub := filepath.Join(parent, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	s.attachWorkspacePinFrom(context.Background(), "file://"+sub, sessionstate.PinSourceSessionStart, pinTriggerRestore)
	if got := s.workspace(); got == parent {
		t.Fatalf("FAIL-OPEN: restore widened the markerless subdirectory pin to the enclosing root %q", got)
	}
	if got := s.workspace(); got != "" {
		t.Fatalf("refused drift still left the session attached to %q", got)
	}
}

// TestAttachWorkspacePinFrom_LiveStillWidensToRoot is the live twin: no test
// in this package previously drove attachWorkspacePinFrom with a markerless
// subdirectory, so the widening it does under a live trigger — the ordinary,
// legitimate behaviour a fresh attach relies on — had no non-regression
// coverage of its own. Added alongside the restore guard rather than assumed.
func TestAttachWorkspacePinFrom_LiveStillWidensToRoot(t *testing.T) {
	store, ss := newOriginStore(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	sub := filepath.Join(parent, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	s.attachWorkspacePinFrom(context.Background(), "file://"+sub, sessionstate.PinSourceSessionStart, pinTriggerLive)
	if got := s.workspace(); got != parent {
		t.Fatalf("a live attach of a markerless subdirectory should still resolve to its enclosing root: got %q, want %q", got, parent)
	}
}

// TestRestoreDriftErr_SyntheticBranchExpression covers the guard added to
// rehydratePin's synthetic branch (conn_persist.go) — but at the primitive,
// not end to end, and this comment states why.
//
// End to end is unreachable: restoreRootIntact's OWN pool.Detect(root) call
// already walks up looking for a .git, same as pool.SynthesiseRoot does. So
// any marker that appears above `root` before rehydratePin runs is caught by
// restoreRootIntact itself — Detect succeeds, detected != root, the pin is
// dropped via the NON-synthetic branch, and the synthetic branch's second
// SynthesiseRoot call is never reached at all. The drift this guard defends
// against needs the filesystem to change in the much narrower window BETWEEN
// restoreRootIntact's internal SynthesiseRoot call and rehydratePin's second
// one — a handful of statements inside one synchronous function call, with no
// injection point this test can reach from the outside. Traced through
// restoreRootIntact (conn_persist.go) and SynthesiseRoot (pool_synthesise.go)
// before writing this rather than assumed; see PLAN-347 item 5. The call is
// kept anyway for the same reason shardFor's identity-not-re-Detect shape is
// kept where it is never exercised by a currently-known race either: defence
// in depth, and consistency with sites 1 and 2's identical contract.
//
// Because the site-3 call is not reachable through rehydratePin today,
// reverting it does not turn any test in this file red — that is reported
// plainly in the PR rather than papered over with a test that would pass for
// a reason other than the one it claims.
func TestRestoreDriftErr_SyntheticBranchExpression(t *testing.T) {
	parent := freshTempDir(t)
	sub := filepath.Join(parent, "sub")
	// resolved is what restoreRootIntact verified (the pin root); synth is
	// what a SECOND SynthesiseRoot call would return if a marker had appeared
	// above it in between — exactly the shape rehydratePin's synthetic branch
	// guards against.
	if err := restoreDriftErr(sub, sub, pinTriggerRestore); err != nil {
		t.Fatalf("identical resolved/synth must not be gated: %v", err)
	}
	if err := restoreDriftErr(sub, parent, pinTriggerRestore); err == nil {
		t.Fatal("a re-synthesis that climbed to the enclosing root must be refused")
	}
}

// TestRestoreDriftErr_TriggerAndIdentity is a table test on the helper
// itself: live is never gated, equal strings are never gated, and only a
// restore trigger with differing strings is gated.
func TestRestoreDriftErr_TriggerAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name               string
		verified, resolved string
		trigger            pinTrigger
		wantErr            bool
	}{
		{"live trigger never gated, even when the strings differ", "/a", "/a/b", pinTriggerLive, false},
		{"equal strings never gated under restore", "/a", "/a", pinTriggerRestore, false},
		{"equal strings never gated under live", "/a", "/a", pinTriggerLive, false},
		{"differing strings gated under restore", "/a/b", "/a", pinTriggerRestore, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := restoreDriftErr(tc.verified, tc.resolved, tc.trigger)
			if tc.wantErr && err == nil {
				t.Fatalf("restoreDriftErr(%q, %q, %q) = nil, want an error", tc.verified, tc.resolved, tc.trigger)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("restoreDriftErr(%q, %q, %q) = %v, want nil", tc.verified, tc.resolved, tc.trigger, err)
			}
		})
	}
}
