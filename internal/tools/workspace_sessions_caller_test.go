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

// A caller that is nobody is shown as nobody. The per-call row is final: the tool
// must not fall back to the connection's row for a caller that has none, which is
// how an unattributable call on a shared connection came to be listed as "you" with
// the owner's sent notes and thread ids.
func TestWorkspaceSessions_ACallerThatIsNobodyIsNotShownAsTheConnection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	owner := registerRow(t, ws, "", "")
	peer := registerRow(t, ws, "", "other")

	tool := NewWorkspaceSessions(func() string { return ws }, func() string { return owner.ID }).
		WithAgentIdentity(func(ctx context.Context) (string, string) {
			if mcp.LogicalAgentFromCtx(ctx) == "owner" {
				return ws, owner.ID
			}
			return ws, ""
		})
	list := func(agent string) string {
		t.Helper()
		out, err := tool.Execute(mcp.WithLogicalAgent(context.Background(), agent), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return out
	}

	own := list("owner")
	for _, want := range []string{"you:  " + owner.Name, owner.Name + " (you)"} {
		if !strings.Contains(own, want) {
			t.Fatalf("the owner's own listing does not say %q, so the absences below prove nothing:\n%s", want, own)
		}
	}

	nobody := list("nobody")
	for _, leak := range []string{"you:", "(you)"} {
		if strings.Contains(nobody, leak) {
			t.Errorf("a caller with no identity was shown as somebody (%q):\n%s", leak, nobody)
		}
	}
	// Not hidden from the roster: it can still see who is here and reach them.
	for _, name := range []string{owner.Name, peer.Name} {
		if !strings.Contains(nobody, name) {
			t.Errorf("the roster a caller with no identity sees is missing %s:\n%s", name, nobody)
		}
	}
}
