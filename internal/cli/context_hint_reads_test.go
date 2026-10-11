package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/contexthints"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// Behaviourally, a hint is not a read: serving one that names a real file, to a
// real connection's agent, leaves that agent's read tracker empty, so a strict
// edit of the file is still refused. The control: after an explicit read_file the
// same edit goes through, so the refusal was the missing read and nothing else
// (cross-review S4; TestContextHintService_HasNoReadTracker is the structural half).
func TestContextHint_ServingAHintRecordsNoRead(t *testing.T) {
	world := newIdentityWorld(t)
	world.cfg.Edits.Strict = true // the read gate under test exists only in strict mode
	root := identityRepo(t)
	file := filepath.Join(root, "cart.go")
	if err := os.WriteFile(file, []byte("package cart\n\nfunc Total() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := world.conn("")
	c.start("conversation", root, "conversation", nil)
	world.registry.add(c.s.sessionID(), connHandle{hintRoot: c.s.hookHintRoot, mailboxInbox: c.s.hookInbox})

	ledger, err := contexthints.OpenAt(filepath.Join(t.TempDir(), "context_hints.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ledger.Close)
	hinter := &fakeHinter{lines: func(tools.HintRequest) []tools.HintLine {
		return []tools.HintLine{{Selector: "Total", Path: "cart.go", Kind: "func", Provenance: "code", Line: 3}}
	}}
	svc := newContextHintService(hinter, ledger, world.registry.hintRoot, func() bool { return true })
	if r := svc.serve(context.Background(), prompt("conversation", "cart.go")); r.Outcome != contexthints.OutcomeEmitted || !strings.Contains(r.Text, "cart.go") {
		t.Fatalf("hint = %+v; the test needs a hint that names the file", r)
	}

	agentCtx := mcp.WithLogicalAgent(context.Background(), "conversation")
	if got := c.s.readTrackerFor(agentCtx).Mtime(file); !got.IsZero() {
		t.Fatalf("serving a hint recorded a read of %s (mtime %v)", file, got)
	}
	// dirty_ok: the file is uncommitted, and that guard must not be what refuses.
	edit := map[string]any{"file_path": file, "dirty_ok": true, "edits": []any{map[string]any{"old_string": "return 1", "new_string": "return 2"}}}
	if text, isErr := c.call("conversation", "edit_file", edit); !isErr || !strings.Contains(text, "read") || strings.Contains(text, "uncommitted") {
		t.Fatalf("a strict edit after only a hint: isErr=%v %q, want the read gate's refusal", isErr, text)
	}
	if text, isErr := c.call("conversation", "read_file", map[string]any{"file_path": file}); isErr {
		t.Fatalf("read_file: %s", text)
	}
	edit["edits"] = []any{map[string]any{"old_string": "return 1", "new_string": "return 2"}}
	if text, isErr := c.call("conversation", "edit_file", edit); isErr {
		t.Fatalf("control: the same edit after a real read was refused: %s", text)
	}
}
