package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

func TestLeaveNote_LongConversationNeverRefused(t *testing.T) {
	const repliers = 24
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	tool := NewLeaveNote(deps)
	conv := put(t, local, "bob", "alice", "opening question", "", "", "")
	if rows, err := local.ClaimNotesForSession(
		context.Background(), "alice", deps.SessionID, deps.Workspace(), time.Now(), 1,
	); err != nil || len(rows) != 1 {
		t.Fatalf("establish alice's participation: rows=%#v err=%v", rows, err)
	}

	var (
		mu       sync.Mutex
		failures []string
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for i := range repliers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			raw := json.RawMessage(fmt.Sprintf(
				`{"to":"bob","body":"reply %d","conversation_id":%s}`, i, jsonStr(conv)))
			out, err := tool.Execute(context.Background(), raw)
			if err != nil || strings.Contains(out, "NOT sent") {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("err=%v out=%q", err, out))
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) != 0 {
		t.Fatalf("%d replies were discarded (first: %s)", len(failures), failures[0])
	}
	if n, err := local.ConversationCount(context.Background(), conv); err != nil || n != repliers+1 {
		t.Fatalf("conversation count = %d, err=%v, want %d; no count cap may discard a note", n, err, repliers+1)
	}
}

func TestLeaveNote_ReportsExactTruncationToBothParties(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, ChatBudgetBytes: 8}, "alice")
	body := "abc😀defghij" // 14 bytes; the eight-byte window ends after d.
	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"to":"bob","body":"abc😀defghij"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"queued for session bob", "pending delivery", "8 of 14 bytes", "configured 8-byte budget",
		"6 bytes were TRUNCATED", "byte offset 8", "write a file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sender receipt missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, body) {
		t.Errorf("sender receipt echoed the full body: %q", out)
	}
	pending, err := local.PendingNotesForSession(context.Background(), "bob", "sess-bob", deps.Workspace(), time.Now())
	if err != nil || len(pending) != 1 || pending[0].Body != "abc😀d" || pending[0].OriginalBytes != len(body) {
		t.Fatalf("durable note was not byte-bounded with exact original size: rows=%#v err=%v", pending, err)
	}

	rows, err := local.ClaimNotesForSession(context.Background(), "bob", "sess-bob", deps.Workspace(), time.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	rendered := RenderMessages(rows, 8, time.Now())
	for _, want := range []string{"abc😀d", "received 8 of 14 bytes", "remaining 6", "shared by path"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("recipient delivery missing %q: %q", want, rendered)
		}
	}
	if strings.Contains(rendered, body) {
		t.Errorf("recipient received beyond the advertised byte window: %q", rendered)
	}
}

func TestLeaveNote_RedactedTruncationDoesNotInventSubmittedBodyOffset(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, ChatBudgetBytes: 16}, "alice")
	body := "prefix AKIAIOSFODNN7EXAMPLE substantive remainder"
	out, err := NewLeaveNote(deps).Execute(context.Background(), json.RawMessage(fmt.Sprintf(
		`{"to":"bob","body":%s}`, jsonStr(body),
	)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"TRUNCATED", "redaction changed the stored representation", "no reliable byte offset",
		"substantive remainder", "conversation_id", "safe file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("redacted truncation receipt missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "resume the original UTF-8 body at byte offset") {
		t.Fatalf("redacted receipt invented an offset into the submitted body: %q", out)
	}
}

func TestRenderMessages_ProvidesReplyMetadataPerNote(t *testing.T) {
	now := time.Now()
	out := RenderMessages([]collab.Row{
		{AuthorSession: "bob", Body: "one", OriginalBytes: 3, ConversationID: "conv-one", CreatedAt: now},
		{AuthorSession: "carol", Body: "two", OriginalBytes: 3, ConversationID: "conv-two", CreatedAt: now},
	}, 8, now)
	for _, conv := range []string{"conv-one", "conv-two"} {
		if strings.Count(out, "conversation_id: "+jsonStr(conv)) != 1 {
			t.Fatalf("reply metadata for %s missing or duplicated: %q", conv, out)
		}
	}
}

func TestLeaveNote_InThreadOmittedTargetResolvesOtherPeer(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	conv := put(t, local, "bob", "alice", "question", "", "", "")
	if rows, err := local.ClaimNotesForSession(
		context.Background(), "alice", deps.SessionID, deps.Workspace(), time.Now(), 1,
	); err != nil || len(rows) != 1 {
		t.Fatalf("establish alice's participation: rows=%#v err=%v", rows, err)
	}

	out, err := NewLeaveNote(deps).Execute(context.Background(),
		json.RawMessage(`{"body":"answer","conversation_id":`+jsonStr(conv)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session bob") {
		t.Fatalf("thread reply did not resolve bob: %q", out)
	}
	rows, err := local.ClaimNotesForSession(context.Background(), "bob", "id-bob", deps.Workspace(), time.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Body != "answer" || rows[0].Addressee != "bob" {
		t.Fatalf("thread reply was misrouted: %#v", rows)
	}
}

func TestLeaveNote_SelfAndUnknownThreadRefusalsConsumeNothing(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "alice")
	tool := NewLeaveNote(deps)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"to":"alice","body":"loop"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "NOT sent") || !strings.Contains(out, "own author") {
		t.Fatalf("self-send was not explicitly refused: %q", out)
	}
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"body":"lost","conversation_id":"unknown"}`)); err == nil {
		t.Fatal("unknown in-thread target must fail closed")
	}
	sent, err := local.RecentSentNotes(context.Background(), deps.SessionID, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Fatalf("refused attempts consumed rows: %#v", sent)
	}
}

func TestLeaveNote_RemainderFollowUpDoesNotRedeliverOriginal(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, ChatBudgetBytes: 4}, "alice")
	tool := NewLeaveNote(deps)
	first, err := tool.Execute(context.Background(), json.RawMessage(`{"to":"bob","body":"abcdefgh"}`))
	if err != nil {
		t.Fatal(err)
	}
	convLine := strings.Split(strings.Split(first, "conversation: ")[1], " ")[0]
	inbox := Inbox{
		Self: "bob", SelfID: "sess-bob", Root: deps.Workspace(),
		Policy:    CollabPolicy{Mailbox: true, ChatBudgetBytes: 4},
		Workspace: func() *collab.Store { return local },
	}
	original := inbox.Claim(context.Background())
	if len(original) != 1 || original[0].Body != "abcd" || original[0].OriginalBytes != 8 {
		t.Fatalf("first claim = %#v", original)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(
		`{"to":"bob","body":"efgh","conversation_id":`+jsonStr(convLine)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	remainder := inbox.Claim(context.Background())
	if len(remainder) != 1 || remainder[0].Body != "efgh" {
		t.Fatalf("remainder claim = %#v", remainder)
	}
	if again := inbox.Claim(context.Background()); len(again) != 0 {
		t.Fatalf("a delivered original or remainder was repeated: %#v", again)
	}
}
