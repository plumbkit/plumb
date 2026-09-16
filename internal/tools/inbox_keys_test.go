package tools

import (
	"testing"

	"github.com/plumbkit/plumb/internal/collab"
)

// TestInbox_KeysIncludeBoundSessionIDs: a note bound to a session ID is
// delivered by ID, so the inbox must be woken by that ID and not only by its
// name. check_messages with a wait blocks on these keys, so a name-only key set
// leaves a bound message unwoken and tells the waiter "No messages" rather than
// merely delaying it.
func TestInbox_KeysIncludeBoundSessionIDs(t *testing.T) {
	inbox := Inbox{Self: "alice", SelfID: "sess-alice", InheritedIDs: []string{"sess-old"}, Root: "/ws"}
	want := map[string]bool{
		"alice":      false,
		"sess-alice": false,
		"sess-old":   false,
		collab.NotifyKey("/ws", collab.AddresseeNext): false,
	}
	for _, k := range inbox.Keys() {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected notifier key %q", k)
			continue
		}
		want[k] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing notifier key %q; a bound message woken only by name leaves a waiting check_messages asleep", k)
		}
	}
	// A session with no name has no mailbox address and therefore no keys.
	if got := (Inbox{}).Keys(); got != nil {
		t.Errorf("an unnamed inbox returned keys %v, want nil", got)
	}
}
