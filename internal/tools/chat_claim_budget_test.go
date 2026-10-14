package tools

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// TestInboxClaimResult_SlowOpenDoesNotSpendTheClaimBudget: opening a store is not
// bounded by the claim's context (a reader takes none), so a store that is slow
// to open must not use up the claim budget before the claim runs. Charged to the
// budget, a slow open turned mail that was readable into "context deadline
// exceeded", which check_messages renders as an incomplete mailbox (PLAN-545:
// TestHookMailbox_InboxKeepsOneRootAcrossRepin under -race on CI).
func TestInboxClaimResult_SlowOpenDoesNotSpendTheClaimBudget(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	put(t, local, "bob", "alice", "after a slow open", "", "", "")
	inbox := Inbox{
		Self: "alice", SelfID: deps.SessionID(), Root: wsRootOf(deps), Policy: deps.Policy(),
		WorkspaceReader: func() (*collab.Store, error) {
			time.Sleep(2 * chatClaimTimeout)
			return deps.StoreIfExists(), nil
		},
	}
	rows, err := inbox.ClaimResult(context.Background())
	if err != nil {
		t.Fatalf("a slow but successful open failed the read: rows=%d err=%v", len(rows), err)
	}
	if len(rows) != 1 || rows[0].Body != "after a slow open" {
		t.Fatalf("rows = %+v, want the one note", rows)
	}
}

// TestInboxSnapshot_SlowOpenDoesNotSpendTheBudget: the hooks' probe observes the
// same stores, so a slow open must not make it report a failure (which the Stop
// hook reads as "no mail") either.
func TestInboxSnapshot_SlowOpenDoesNotSpendTheBudget(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	put(t, local, "bob", "alice", "seen after a slow open", "", "", "")
	inbox := Inbox{
		Self: "alice", SelfID: deps.SessionID(), Root: wsRootOf(deps), Policy: deps.Policy(),
		WorkspaceReader: func() (*collab.Store, error) {
			time.Sleep(2 * chatClaimTimeout)
			return deps.StoreIfExists(), nil
		},
	}
	snap, err := inbox.Snapshot(context.Background())
	if err != nil || snap.Count != 1 {
		t.Fatalf("snapshot after a slow open = %+v err=%v, want one pending note", snap, err)
	}
}

// TestInboxClaimResult_CancelledCallerStillFails: the claim budget no longer
// covers the open, but a caller whose own context is already done still gets
// its error and no delivery.
func TestInboxClaimResult_CancelledCallerStillFails(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	put(t, local, "bob", "alice", "never claimed", "", "", "")
	inbox := Inbox{
		Self: "alice", SelfID: deps.SessionID(), Root: wsRootOf(deps), Policy: deps.Policy(),
		Workspace: deps.StoreIfExists,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rows, err := inbox.ClaimResult(ctx); err == nil || len(rows) != 0 {
		t.Fatalf("a cancelled caller read rows=%d err=%v; want an error and nothing claimed", len(rows), err)
	}
	// Control: the note is still there for the next, live read.
	rows, err := inbox.ClaimResult(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("the next read got rows=%d err=%v, want the one note", len(rows), err)
	}
}
