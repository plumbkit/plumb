package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// chat_preview_rename_test.go is the preview surface's half of the
// bound-mail-follows-the-identity rule.
//
// Inbox.Peek reads through collab.ClaimableNotes, which is built on the same
// claimable() predicate as delivery — so widening delivery to a bound row whose
// addressee NAME no longer matches widens the preview with it. That is intended:
// a preview blind to the session's own bound mail leaves an agent with no early
// signal for exactly the message it is waiting on. It is asserted here rather
// than inferred from the shared predicate, because "they share a function" is a
// fact about today's code and not a guarantee.

// TestPeek_PreviewsMailBoundToThisSessionAfterARename: same session, same
// internal ID, regenerated display name — the state a degraded identity
// recovery leaves behind. The preview must still show it.
func TestPeek_PreviewsMailBoundToThisSessionAfterARename(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "icy-beaver")

	if _, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "ancient-stag", AuthorID: "id-stag",
		Body:      "written while it answered to gentle-mink",
		Addressee: "gentle-mink", AddresseeID: deps.sessionID(),
		TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	previews := Inbox{
		Self: "icy-beaver", SelfID: deps.sessionID(), Root: deps.Workspace(),
		Policy:    CollabPolicy{Mailbox: true},
		Workspace: deps.StoreIfExists, Global: deps.GlobalStoreIfExists,
	}.Peek(context.Background())
	var joined strings.Builder
	for _, p := range previews {
		joined.WriteString(p.Row.Body)
		joined.WriteString("\n")
	}
	if !strings.Contains(joined.String(), "written while it answered to gentle-mink") {
		t.Fatalf("the preview did not show mail bound to this session's own ID after a rename; got %q", joined.String())
	}
}

// TestPeek_DoesNotPreviewAnotherSessionsBoundMail is the baseline the widening
// must not cost. Holding the addressee's NAME is not entitlement to see the
// body — which on the preview path matters more than on the claim, since the
// preview rides an ordinary tool result the agent did not ask for.
func TestPeek_DoesNotPreviewAnotherSessionsBoundMail(t *testing.T) {
	deps, local, _ := chatTestDeps(t, CollabPolicy{Mailbox: true}, "gentle-mink")

	if _, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "ancient-stag", AuthorID: "id-stag",
		Body:      "secret-body-marker",
		Addressee: "gentle-mink", AddresseeID: "sess-somebody-else",
		TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	previews := Inbox{
		Self: "gentle-mink", SelfID: deps.sessionID(), Root: deps.Workspace(),
		Policy:    CollabPolicy{Mailbox: true},
		Workspace: deps.StoreIfExists, Global: deps.GlobalStoreIfExists,
	}.Peek(context.Background())
	for _, p := range previews {
		if strings.Contains(p.Row.Body, "secret-body-marker") {
			t.Fatalf("the preview disclosed a note bound to another session: %q", p.Row.Body)
		}
	}
}
