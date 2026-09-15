//go:build integration

package smoke_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mailbox_conformance_test.go is the end-to-end tier of the mailbox contract:
// four real `plumb serve` proxies, one daemon, one workspace, messages crossing
// the actual MCP transport.
//
// The unit tests in internal/cli and internal/tools cover each rule in
// isolation, calling messageHint and reading its return value. What this tier
// adds is the WIRE: the block is serialised by the daemon, carried as MCP
// content, and reassembled by a real serve proxy before anything asserts on it.
// That is not a formality — the one defect this contract has shipped since
// (a reconnect note welding onto the preview's last line, with no separator
// between two correct content items) existed only in that assembly step and was
// invisible to every in-process test.
//
// A note on what these tests do NOT prove, because the obvious claim is wrong
// and worth writing down before someone repeats it: they do not "discard" the
// block the way a sandboxed-program harness does. A Go test cannot un-see a
// string. What makes the delivery assertion valid is narrower and sufficient —
// check_messages is not called until after the preview has gone out, so if the
// preview had claimed, the message would be gone by then regardless of who read
// what.
//
// The "next" race needs four sessions and cannot be staged with fewer. A note
// addressed to "next" has one winner and several candidates, and the hazard a
// non-consuming preview introduces is that every candidate is shown the body
// and acts on it while the store records a single recipient. Two sessions
// (author plus one candidate) cannot tell "exactly one winner" from "everyone
// wins"; three candidates can.

// startMailboxSessions brings up n proxies sharing one daemon and one
// workspace, and returns them with their display names — the addresses notes
// are written to.
func startMailboxSessions(t *testing.T, ctx context.Context, plumbBin, tmpHome, fixture string, n int) ([]*mcpClient, []string) {
	t.Helper()
	clients := make([]*mcpClient, n)
	names := make([]string, n)
	for i := range n {
		c := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
		c.initialize(t, fixture)
		packet := c.call(t, "session_start", map[string]any{
			"workspace":  fixture,
			"session_id": fmt.Sprintf("mailbox-conformance-%d", i),
		}, sessionStartTimeout)
		id := parseSelfIdentity(packet)
		if id.name == "" {
			t.Fatalf("session %d: orientation packet named no session:\n%s", i, packet)
		}
		clients[i], names[i] = c, id.name
	}
	return clients, names
}

// TestSmoke_Mailbox_PreviewDoesNotConsume is the regression for the defect that
// made the preview necessary: a message claimed by a tool result the client
// discards is a message nobody can ever read again.
//
// The discard is the point of the test. `read_file`'s result carries the block
// and is thrown away here without being parsed — the strongest available stand-in
// for a client whose model never sees it — and check_messages is then asked, as
// the only authoritative view, whether the message survived.
func TestSmoke_Mailbox_PreviewDoesNotConsume(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeMarkerFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	clients, names := startMailboxSessions(t, ctx, plumbBin, tmpHome, fixture, 2)
	sender, recipient := clients[0], clients[1]
	shared := filepath.Join(fixture, "shared.txt")
	const body = "the deploy is blocked, stop what you are doing"

	t.Log("A: leave_note -> B")
	sent := sender.call(t, "leave_note", map[string]any{"to": names[1], "body": body}, toolTimeout)
	assertContains(t, "leave_note", sent, "Message sent")

	// B's next call is an ordinary tool, and the preview rides its result across
	// the wire. Nothing here takes delivery: check_messages is not called until
	// after these assertions, which is what makes the survival check below mean
	// something.
	t.Log("B: read_file (the preview rides this result)")
	previewed := recipient.call(t, "read_file", map[string]any{"file_path": shared}, toolTimeout)
	assertContains(t, "preview rides on the result", previewed, body)
	assertContains(t, "preview says what it is", previewed, "Preview:")
	if strings.Contains(previewed, "reply: leave_note") {
		t.Errorf("a preview must withhold the reply handle — replying would leave the note unread:\n%s", previewed)
	}

	// The sender must still see it unread: nothing has taken delivery.
	outbox := sender.call(t, "check_messages", map[string]any{}, toolTimeout)
	assertContains(t, "sender's outbox after a preview", outbox, "NOBODY has read yet")

	// The authoritative view still has it.
	t.Log("B: check_messages (the only channel that delivers)")
	got := recipient.call(t, "check_messages", map[string]any{}, toolTimeout)
	assertContains(t, "check_messages delivers the previewed note", got, body)
	assertContains(t, "delivery carries the reply handle", got, "reply: leave_note")

	// And exactly once.
	again := recipient.call(t, "check_messages", map[string]any{}, toolTimeout)
	if strings.Contains(again, body) {
		t.Errorf("a claimed message must not be delivered twice:\n%s", again)
	}

	// Only now does the sender's outbox clear — "delivered" means delivered.
	cleared := sender.call(t, "check_messages", map[string]any{}, toolTimeout)
	if strings.Contains(cleared, "NOBODY has read yet") {
		t.Errorf("the outbox must clear once the recipient takes delivery:\n%s", cleared)
	}
}

// TestSmoke_Mailbox_NextNoteHasExactlyOneWinner covers the hazard the preview
// introduces, with the session count it actually requires.
//
// "next" is leave_note's DEFAULT addressee. While delivery and claiming were
// one act the atomic UPDATE settled it: one session was handed the body, one
// session acted. A preview claims nothing, so showing the body to each
// candidate would invite all three to carry out an instruction the store
// records as going to one. The preview therefore states a COUNT and withholds
// the body, and the claim still picks a single winner.
func TestSmoke_Mailbox_NextNoteHasExactlyOneWinner(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeMarkerFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	clients, _ := startMailboxSessions(t, ctx, plumbBin, tmpHome, fixture, 4)
	author, candidates := clients[0], clients[1:]
	shared := filepath.Join(fixture, "shared.txt")
	const body = "rebase the release branch"

	t.Log("A: leave_note -> next")
	author.call(t, "leave_note", map[string]any{"to": "next", "body": body}, toolTimeout)

	// Every candidate previews BEFORE anyone claims, which is the ordering the
	// hazard lives in: all three are looking at the same unclaimed note.
	for i, c := range candidates {
		out := c.call(t, "read_file", map[string]any{"file_path": shared}, toolTimeout)
		if strings.Contains(out, body) {
			t.Errorf("candidate %d was shown a \"next\" body it may not win:\n%s", i, out)
		}
		assertContains(t, fmt.Sprintf("candidate %d is told one waits", i), out, "whoever attaches next")
		// The count must arrive as plumb speaking, not appended mid-line onto the
		// tool's own output — a preview of only "next" notes renders no bodies, so
		// nothing else supplies the header.
		assertContains(t, fmt.Sprintf("candidate %d count carries its header", i), out, "[Messages —")
	}

	// Exactly one claim may succeed.
	winners := 0
	for i, c := range candidates {
		if strings.Contains(c.call(t, "check_messages", map[string]any{}, toolTimeout), body) {
			winners++
			t.Logf("candidate %d won the claim", i)
		}
	}
	if winners != 1 {
		t.Fatalf("a \"next\" note must have exactly one winner across %d candidates; got %d",
			len(candidates), winners)
	}
}
