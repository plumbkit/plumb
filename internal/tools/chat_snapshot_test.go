package tools

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

func TestInboxSnapshot_CrossProjectConsentAndDeliveryAgree(t *testing.T) {
	deps, local, global := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	put(t, local, "bob", "alice", "same workspace", "", "", "")
	put(t, global, "carol", "alice", "cross project", "", "/sender", wsRootOf(deps))
	put(t, global, "carol", "alice", "wrong workspace", "", "/sender", "/elsewhere")
	inbox := Inbox{
		Self: "alice", SelfID: deps.SessionID(), Root: wsRootOf(deps),
		Policy: deps.Policy(), Workspace: deps.StoreIfExists, Global: deps.GlobalStoreIfExists,
	}
	ctx := context.Background()
	off, err := inbox.Snapshot(ctx)
	if err != nil || off.Count != 1 {
		t.Fatalf("unconsenting recipient saw cross-project mail: %+v err=%v", off, err)
	}
	inbox.Policy.CrossProject = true
	on, err := inbox.Snapshot(ctx)
	if err != nil || on.Count != 2 || on.Fingerprint == off.Fingerprint {
		t.Fatalf("consenting snapshot=%+v err=%v", on, err)
	}
	rows, err := inbox.ClaimResult(ctx)
	if err != nil || len(rows) != on.Count {
		t.Fatalf("metadata did not match actual delivery: rows=%d err=%v", len(rows), err)
	}
	empty, err := inbox.Snapshot(ctx)
	if err != nil || empty.Count != 0 {
		t.Fatalf("delivered mail still pending: %+v err=%v", empty, err)
	}
	// The wrong workspace's note stays unread; the probe/claim never spends it.
	remaining, err := global.ClaimableNotes(ctx, collab.Claimant{Name: "alice", ID: deps.SessionID(), Workspace: "/elsewhere"}, time.Now(), 0)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("foreign mailbox altered: remaining=%d err=%v", len(remaining), err)
	}
}
