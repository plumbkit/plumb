package tools

// session_start_mail_mode_test.go — PLAN-496. session_start DELIVERS mail by
// claiming it, and a client that calls the tool automatically (a hook, or a
// plugin that does it on nearly every connection) therefore consumes notes
// before its model ever reads them. The opt-in `mail: "preview"` shows the same
// notes and claims none of them.
//
// The tests below are about the two halves of the mailbox contract, not about
// the rendered packet alone: what session_start shows AND what check_messages
// can still deliver afterwards are asserted together, because only the second
// half tells a preview apart from a claim.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/collab"
)

// mailModeHarness wires ONE workspace with a real collab store, a session_start
// that delivers from it, and the check_messages that shares the same inbox. A
// test can then make a session_start call and ask what mail is left for
// check_messages, which is the only way to observe a claim that should not have
// happened.
func mailModeHarness(t *testing.T) (tool *SessionStart, check *CheckMessages, store *collab.Store) {
	t.Helper()
	const self = "gentle-mink"
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, self)
	ws := deps.Workspace()
	policy := CollabPolicy{Mailbox: true}
	tool = NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithMailbox(func() (bool, Inbox) {
			return true, Inbox{
				Self: self, SelfID: deps.sessionID(), Root: ws,
				Policy:    policy,
				Workspace: deps.StoreIfExists, Global: deps.GlobalStoreIfExists,
			}
		})
	return tool, NewCheckMessages(deps), local
}

// waitingMail puts one note from a peer, addressed to the harness session by
// name. The empty conversation id starts the thread, exactly as a first note
// from a peer does in the daemon.
func waitingMail(t *testing.T, store *collab.Store, marker string) {
	t.Helper()
	put(t, store, "ancient-stag", "gentle-mink", marker, "", "", "")
}

// TestSessionStart_DefaultMailIsClaimed is the compatibility control, and it has
// to hold for every client that sends no `mail` argument: the packet carries the
// note AND the note is gone from the mailbox afterwards. A change that made the
// default a preview would silently start re-delivering mail these clients have
// already acted on.
func TestSessionStart_DefaultMailIsClaimed(t *testing.T) {
	tool, check, store := mailModeHarness(t)
	const marker = "compat-control-marker"
	waitingMail(t, store, marker)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"detail":"brief"}`))
	if err != nil {
		t.Fatalf("session_start: %v", err)
	}
	if !strings.Contains(out, marker) {
		t.Fatalf("the default packet must deliver waiting mail; got:\n%s", out)
	}

	again, err := check.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("check_messages: %v", err)
	}
	if strings.Contains(again, marker) {
		t.Errorf("the note was delivered twice — session_start claimed it, yet check_messages still returned it:\n%s", again)
	}
	if !strings.Contains(again, "No messages.") {
		t.Errorf("expected an empty mailbox after the default claim; got:\n%s", again)
	}
}

// TestSessionStart_MailPreviewDoesNotClaim is the fix. The packet shows the
// waiting note with the preview wording, and — the half that matters — the note
// is STILL delivered by check_messages afterwards. Asserting only the packet's
// contents would pass just as well if the preview had quietly claimed.
func TestSessionStart_MailPreviewDoesNotClaim(t *testing.T) {
	tool, check, store := mailModeHarness(t)
	const marker = "preview-mode-marker"
	waitingMail(t, store, marker)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"mail":"preview","detail":"brief"}`))
	if err != nil {
		t.Fatalf("session_start: %v", err)
	}
	if !strings.Contains(out, marker) {
		t.Fatalf("the preview packet must show what is waiting; got:\n%s", out)
	}
	if !strings.Contains(out, "from ancient-stag") {
		t.Errorf("the preview must name the sender as the delivery rendering does; got:\n%s", out)
	}
	// The preview's own wording, and the absence of the delivery wording: a
	// packet must not read as if it had handed the note over.
	if !strings.Contains(out, "waiting") || !strings.Contains(out, "NOT marked read") {
		t.Errorf("the preview packet does not say the note is unread and waiting; got:\n%s", out)
	}
	for _, claimed := range []string{"1 new, addressed to you", "reply: leave_note"} {
		if strings.Contains(out, claimed) {
			t.Errorf("the preview packet claims delivery (%q) it did not make; got:\n%s", claimed, out)
		}
	}

	after, err := check.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("check_messages: %v", err)
	}
	if !strings.Contains(after, marker) {
		t.Fatalf("mail:\"preview\" consumed the note: check_messages delivered nothing:\n%s", after)
	}
	if !strings.Contains(after, "reply: leave_note") {
		t.Errorf("the later delivery must carry the reply handle the preview withheld; got:\n%s", after)
	}
}

// TestSessionStart_UnknownMailModeIsRefused pins the third answer: a value that
// is neither mode is an error naming both, never a silent fall back to one of
// them — falling back to the default would consume mail a caller had asked to
// leave alone.
func TestSessionStart_UnknownMailModeIsRefused(t *testing.T) {
	tool, check, store := mailModeHarness(t)
	const marker = "refused-mode-marker"
	waitingMail(t, store, marker)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"mail":"nope","detail":"brief"}`))
	if err == nil {
		t.Fatal("an unrecognised mail mode must be refused, not treated as a default")
	}
	for _, want := range []string{`"claim"`, `"preview"`, `"nope"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %s; got %q", want, err.Error())
		}
	}

	// A refused call commits nothing, this note included: it must still be there
	// for the call that asks for it properly.
	after, err := check.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("check_messages: %v", err)
	}
	if !strings.Contains(after, marker) {
		t.Fatalf("the refused call claimed mail anyway; check_messages got:\n%s", after)
	}
}
