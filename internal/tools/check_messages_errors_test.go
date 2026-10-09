package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/collab"
)

func TestCheckMessages_ReadFailureIsNotEmpty(t *testing.T) {
	for _, failure := range []string{"open", "query", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
			ctx := context.Background()
			switch failure {
			case "open":
				deps.StoreReader = func(context.Context) (*collab.Store, error) {
					return nil, errors.New("injected mailbox open failure")
				}
			case "query":
				_ = local.Close()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			out, err := NewCheckMessages(deps).Execute(ctx, nil)
			if err == nil || strings.Contains(out, "No messages") || !strings.Contains(err.Error(), "Mailbox unavailable") {
				t.Fatalf("failed %s read asserted empty: out=%q err=%v", failure, out, err)
			}
		})
	}
}

func TestCheckMessages_PartialFailurePreservesDelivery(t *testing.T) {
	deps, local, global := chatTestDeps(t, CollabPolicy{Mailbox: true, CrossProject: true}, "alice")
	put(t, local, "bob", "alice", "deliver this exactly once", "", "", "")
	_ = global.Close()
	tool := NewCheckMessages(deps)
	out, err := tool.Execute(context.Background(), nil)
	if err != nil || !strings.Contains(out, "deliver this exactly once") || !strings.Contains(out, "Mailbox unavailable or incomplete") {
		t.Fatalf("partial delivery hidden: out=%q err=%v", out, err)
	}
	out, err = tool.Execute(context.Background(), nil)
	if err == nil || strings.Contains(out, "deliver this exactly once") || strings.Contains(out, "No messages") {
		t.Fatalf("partial failure repeated delivery or asserted empty: out=%q err=%v", out, err)
	}
}

func TestInbox_AdvisoryFailureDoesNotFailUnrelatedTool(t *testing.T) {
	inbox := Inbox{
		Self: "alice", SelfID: "id-alice", Policy: CollabPolicy{Mailbox: true},
		WorkspaceReader: func() (*collab.Store, error) { return nil, errors.New("offline") },
	}
	if rows := inbox.Claim(context.Background()); len(rows) != 0 {
		t.Fatalf("failed advisory read delivered %v", rows)
	}
	if _, err := inbox.ClaimResult(context.Background()); err == nil {
		t.Fatal("explicit read lost its failure")
	}
}
