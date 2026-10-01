package tools

// workspace_sessions_caller_test.go — the listing's mail block is the CALLER's
// (#556). It prints the sender and body of every pending note it finds, so an
// agent handed another agent's name, ID or predecessor identities would read that
// agent's mail without consuming a byte of it.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/mcp"
)

func TestWorkspaceSessions_MailBlockIsTheCallers(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("open collab store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// To the PARENT, bound to the session it had before a restart.
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "parent-bound-marker",
		Addressee: "parent", AddresseeID: "sess-parent-before", TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatalf("PutNote: %v", err)
	}
	// To the PARENT by name alone.
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "id-bob", Body: "parent-unbound-marker",
		Addressee: "parent", TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatalf("PutNote: %v", err)
	}

	name := map[string]string{"owner": "parent", "sub": "child", "nobody": ""}
	id := map[string]string{"owner": "sess-parent", "sub": "sess-child", "nobody": ""}
	inherited := map[string][]string{"owner": {"sess-parent-before"}}
	listFor := func(agent string) string {
		t.Helper()
		// The connection-level accessors name the OWNER, as a connection does: a
		// caller that is not the owner must not be answered from them.
		tool := NewWorkspaceSessions(func() string { return ws }, func() string { return "sess-parent" }).
			WithAgentName(func(ctx context.Context) string { return name[mcp.LogicalAgentFromCtx(ctx)] }).
			WithAgentIdentity(func(ctx context.Context) (string, string) { return ws, id[mcp.LogicalAgentFromCtx(ctx)] }).
			WithInheritedSessionsFor(func(ctx context.Context) []string { return inherited[mcp.LogicalAgentFromCtx(ctx)] }).
			WithCollab(
				func() (bool, bool) { return false, true },
				func() *collab.Store { return store },
				func() string { return "parent" })
		out, err := tool.Execute(mcp.WithLogicalAgent(context.Background(), agent), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return out
	}

	owner := listFor("owner")
	for _, marker := range []string{"parent-bound-marker", "parent-unbound-marker"} {
		if !strings.Contains(owner, marker) {
			t.Errorf("the owner is not shown its own note %q:\n%s", marker, owner)
		}
	}
	for _, agent := range []string{"sub", "nobody"} {
		out := listFor(agent)
		for _, marker := range []string{"parent-bound-marker", "parent-unbound-marker"} {
			if strings.Contains(out, marker) {
				t.Errorf("%s was shown the parent's note %q:\n%s", agent, marker, out)
			}
		}
	}
}
