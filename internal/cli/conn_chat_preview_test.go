package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
)

// conn_chat_preview_test.go pins the one thing the piggyback must never do:
// consume a message on a channel nobody can prove the model ever saw.
//
// The defect these cover was reported from a DSH (DeepSeek Harness) session and
// reproduced end-to-end against a live daemon. In that harness the agent's only
// directly callable tool is `run_code`; plumb's tools are called from inside a
// sandboxed TypeScript program, and the runtime surfaces to the model ONLY what
// that program prints or returns — every inner tool result is discarded. plumb
// appended the message block to one of those inner results and, in the same
// breath, stamped the row delivered. check_messages then correctly reported an
// empty mailbox on eight consecutive polls, the sender's outbox showed
// "delivered", and the message was gone.

// TestMessageHint_PiggybackDoesNotConsumeTheMessage is the regression. The
// piggyback may PREVIEW a message on an unrelated tool's result — that is the
// fast path, and it is worth keeping — but it must not be what marks the message
// read. Only a tool the model itself called can do that, because only there is
// the result the answer to a question the model asked.
func TestMessageHint_PiggybackDoesNotConsumeTheMessage(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", "alice", "the deploy is blocked, stop what you are doing")

	// The recipient's first call after the send is an ordinary tool, exactly as it
	// is when a run_code program opens with a nested read. Its text is discarded
	// here on purpose: that is what the harness does with it.
	_ = s.enrichToolOutput(context.Background(), "read_file", json.RawMessage(`{}`), "file contents\n")

	// check_messages claims through this same Inbox. It is the model's only
	// authoritative view, so it must still have the message to hand over.
	rows := s.inbox().Claim(context.Background())
	if len(rows) != 1 {
		t.Fatalf("after a piggyback preview the message must still be claimable by check_messages; claimed %d", len(rows))
	}
	if !strings.Contains(rows[0].Body, "the deploy is blocked") {
		t.Errorf("wrong message survived: %q", rows[0].Body)
	}
}

// TestMessageHint_PreviewIsOfferedOnceNotEveryCall guards the other side of the
// same change. A preview that claims nothing would, left alone, re-render on
// every subsequent tool call for as long as the note stays unread — turning one
// message into a banner pasted onto every result. Suppression is per connection
// and in memory, so it costs nothing and cannot itself lose a message: the store
// still holds the note, and check_messages still delivers it.
func TestMessageHint_PreviewIsOfferedOnceNotEveryCall(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", "alice", "only preview me once")

	first := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "On branch main\n")
	if !strings.Contains(first, "only preview me once") {
		t.Fatalf("the first tool result must carry the preview; got %q", first)
	}
	second := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "On branch main\n")
	if strings.Contains(second, "only preview me once") {
		t.Errorf("a previewed message must not be pasted onto every later result; got %q", second)
	}
	// Still unread in the store, which is the whole point.
	if rows := s.inbox().Claim(context.Background()); len(rows) != 1 {
		t.Errorf("suppressing the repeat must not consume the note; claimed %d", len(rows))
	}
}

// TestMessageHint_NextNoteIsCountedNotShown is the hazard a non-consuming
// preview introduces and has to answer for.
//
// "next" means "whoever attaches to this workspace next", and it is leave_note's
// DEFAULT addressee. While delivery and claiming were the same act, the atomic
// UPDATE settled it: one session won, one session acted. A preview claims
// nothing, so printing the body would show one instruction to every session here
// and invite all of them to carry it out, while the store still records a single
// recipient. workspace_sessions' listing refuses to show "next" notes for this
// exact reason (collab.Store.PendingNotes); a preview is that listing with the
// body attached, so it refuses too, and says only that one is waiting.
func TestMessageHint_NextNoteIsCountedNotShown(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", collab.AddresseeNext, "rebase the release branch")

	got := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "On branch main\n")
	if strings.Contains(got, "rebase the release branch") {
		t.Errorf("a \"next\" note's body must not be previewed to a session that may not win it; got %q", got)
	}
	if !strings.Contains(got, "whoever attaches next") {
		t.Errorf("the agent must still learn one is waiting; got %q", got)
	}
	// And it is still there for the claim, which is what picks the one winner.
	if rows := s.inbox().Claim(context.Background()); len(rows) != 1 {
		t.Errorf("the \"next\" note must remain claimable; claimed %d", len(rows))
	}
}

// TestMessageHint_ClaimedMessageStopsBeingPreviewed closes the loop: once the
// model-visible channel has taken the message, the preview has nothing left to
// offer and must fall silent. Without this the fix would trade a lost message
// for a message that is announced forever.
func TestMessageHint_ClaimedMessageStopsBeingPreviewed(t *testing.T) {
	ws := t.TempDir()
	s := newChatTestSession(t, ws, "alice", config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512})
	seedMessage(t, s, ws, "bob", "alice", "read and done")

	if rows := s.inbox().Claim(context.Background()); len(rows) != 1 {
		t.Fatalf("check_messages should claim the seeded note; claimed %d", len(rows))
	}
	// A fresh connection-side view: nothing cached, nothing previewed yet. The
	// store is the only thing that can still be holding the note, and it is not.
	s.chatWatch.reset()
	if got := s.enrichToolOutput(context.Background(), "git", json.RawMessage(`{}`), "OUT"); strings.Contains(got, "read and done") {
		t.Errorf("a claimed message must never be previewed again; got %q", got)
	}
}
