package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// putExpired stores a message that has already aged out — written two hours ago
// with the shortest TTL the store allows — so a test can exercise the
// expired-but-not-yet-pruned window without waiting for one.
func putExpired(t *testing.T, s *collab.Store, from, to, body, conv string) string {
	t.Helper()
	id, err := s.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: from, AuthorID: "id-" + from, Body: body, Addressee: to,
		TTL: time.Minute, ConversationID: conv,
	}, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("PutNote: %v", err)
	}
	return id
}

// TestLeaveNote_BudgetHoldsUnderConcurrentReplies is the tool-level half of the
// check-then-insert fix. leave_note used to count the conversation and then send
// as two steps, so two agents replying into one thread at the same instant both
// passed a count neither had yet invalidated and the cap over-ran — which is
// precisely the runaway exchange [collab] max_exchanges exists to stop. The
// store now enforces the cap inside the insert; this pins that the tool still
// refuses the losers with its own guidance instead of erroring at them.
func TestLeaveNote_BudgetHoldsUnderConcurrentReplies(t *testing.T) {
	const limit, repliers = 8, 24
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxExchanges: limit}, "alice")
	tool := NewLeaveNote(deps)

	conv := put(t, local, "bob", "alice", "opening question", "", "", "")

	var (
		mu       sync.Mutex
		refusals []string
		sent     int
		errs     []error
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for range repliers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := tool.Execute(context.Background(),
				json.RawMessage(`{"to":"bob","body":"reply","conversation_id":`+jsonStr(conv)+`}`))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case strings.Contains(out, "NOT sent"):
				refusals = append(refusals, out)
			default:
				sent++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d concurrent replies errored (first: %v) — a spent budget is a refusal to the "+
			"agent, not a tool failure", len(errs), errs[0])
	}
	n, err := local.ConversationCount(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	if n != limit {
		t.Errorf("conversation holds %d messages, want exactly the %d-message cap", n, limit)
	}
	if sent != limit-1 {
		t.Errorf("%d replies were accepted, want %d — the rest must be refused, not stored", sent, limit-1)
	}
	// The refusal is the only thing the losing agent sees, so its guidance has to
	// survive the move of the check into the store.
	if len(refusals) == 0 {
		t.Fatal("no reply was refused; the budget never bit")
	}
	for _, want := range []string{"max_exchanges", "NOT sent", "human", "rather than opening a fresh thread"} {
		if !strings.Contains(refusals[0], want) {
			t.Errorf("refusal is missing %q; got %q", want, refusals[0])
		}
	}
}

// TestLeaveNote_BudgetIgnoresExpiredMessages: a thread whose messages have aged
// out is not still spent. Counting expired-but-unpruned rows would tie the
// remaining allowance to reaper timing — refused now, allowed a tick later once
// the reaper deletes the very rows that caused the refusal.
func TestLeaveNote_BudgetIgnoresExpiredMessages(t *testing.T) {
	const limit = 2
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxExchanges: limit}, "alice")

	conv := putExpired(t, local, "bob", "alice", "old question", "")
	putExpired(t, local, "alice", "bob", "old answer", conv)

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"still there?","conversation_id":`+jsonStr(conv)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "NOT sent") {
		t.Errorf("expired messages must not hold a thread's budget hostage until the reaper runs; got %q", out)
	}
}
