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
// isolation. What they cannot cover is the property the whole design turns on —
// that a message survives a client which never shows the appended block to its
// model — because in-process they call messageHint and read its return value,
// which IS the surfacing. Here the block goes out over the wire and the test
// simply throws it away, exactly as a harness that runs plumb's tools inside a
// sandboxed program does. Only then does check_messages get asked whether the
// message is still there.
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

	// B's next call is an ordinary tool. Its result carries the preview; this
	// test discards it, which is the whole scenario.
	t.Log("B: read_file (result discarded, as a sandboxed-program harness does)")
	discarded := recipient.call(t, "read_file", map[string]any{"file_path": shared}, toolTimeout)
	assertContains(t, "preview rides on the result", discarded, body)
	assertContains(t, "preview says what it is", discarded, "Preview:")
	if strings.Contains(discarded, "reply: leave_note") {
		t.Errorf("a preview must withhold the reply handle — replying would leave the note unread:\n%s", discarded)
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
