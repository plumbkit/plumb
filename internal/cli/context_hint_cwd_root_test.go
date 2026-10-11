package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/contexthints"
	"github.com/plumbkit/plumb/internal/tools"
)

// The first prompt of a session comes before its first plumb call, so no
// connection knows the session yet. A hint still reaches it when its hook runs in
// a workspace a live connection serves: labelled as coming from the working
// directory, recorded as "cwd-root", and leaving the connection's pin as it was
// and the session still unknown to it (the fallback creates no shard). The
// control: the same request naming a workspace no connection serves is no root,
// and the collector is never asked.
func TestContextHint_FirstPromptUsesAServedWorkingDirectory(t *testing.T) {
	world := newIdentityWorld(t)
	root := identityRepo(t)
	c := world.conn("")
	c.start("conversation", root, "conversation", nil)
	world.registry.add(c.s.sessionID(), connHandle{workspace: c.s.workspace, hintRoot: c.s.hookHintRoot, mailboxInbox: c.s.hookInbox})
	pinned := c.s.workspace()

	ledger, err := contexthints.OpenAt(t.TempDir() + "/context_hints.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ledger.Close)
	hinter := &fakeHinter{lines: func(tools.HintRequest) []tools.HintLine {
		return []tools.HintLine{{Selector: "Total", Path: "cart.go", Kind: "func", Provenance: "code", Line: 3}}
	}}
	svc := newContextHintService(hinter, ledger, world.registry.hintRootFor, func() bool { return true })

	unserved := prompt("fresh", "Total")
	unserved.Workspace = identityRepo(t)
	if r := svc.serve(context.Background(), unserved); r.Outcome != contexthints.OutcomeNoop || len(hinter.calls) != 0 {
		t.Fatalf("an unserved workspace: %+v after %d collector calls, want a noop that asks nothing", r, len(hinter.calls))
	}

	req := prompt("fresh", "Total")
	req.Workspace = root
	r := svc.serve(context.Background(), req)
	if r.Outcome != contexthints.OutcomeEmitted || !strings.HasPrefix(r.Text, hintHeader+cwdRootNote) {
		t.Fatalf("first prompt in a served workspace: %+v, want a hint labelled with the working-directory root", r)
	}
	if len(hinter.calls) != 1 || hinter.calls[0].Workspace != pinned {
		t.Fatalf("collector calls = %+v, want one for the served root %s", hinter.calls, pinned)
	}
	rows, err := ledger.Since(time.Time{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("ledger rows = %+v, %v; want two", rows, err)
	}
	if got := rows[1]; got.Outcome != contexthints.OutcomeEmitted || got.Detail != "cwd-root" || got.Workspace != pinned {
		t.Errorf("ledger row = %+v, want emitted, detail cwd-root, workspace %s", got, pinned)
	}
	if got := rows[0]; got.Detail != "no-root" || got.Workspace != "" {
		t.Errorf("unserved row = %+v, want no-root with no workspace", got)
	}

	if c.s.workspace() != pinned {
		t.Errorf("the connection's pin moved from %s to %s", pinned, c.s.workspace())
	}
	if root, ok := world.registry.hintRoot("fresh"); ok {
		t.Errorf("the fallback left the session known to the connection, at %+v", root)
	}
}
