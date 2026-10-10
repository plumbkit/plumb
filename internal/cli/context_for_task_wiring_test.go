package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
)

// contextTool is the part of the MCP tool contract these tests use.
type contextTool interface {
	InputSchema() json.RawMessage
	Execute(context.Context, json.RawMessage) (string, error)
}

// contextForTaskTool returns the tool exactly as registerAllTools built it.
func contextForTaskTool(t *testing.T) (*connSession, contextTool) {
	t.Helper()
	s, srv := buildTestConnSession(t)
	tool, ok := srv.Lookup("context_for_task")
	if !ok {
		t.Fatal("context_for_task is not registered")
	}
	return s, tool
}

// context_for_task ships experimental: found by name, never pushed. Pinning it
// would pay its schema on every connection, and putting it in the lean set would
// advertise it to clients that have not asked.
func TestContextForTask_IsRegisteredUnpinnedAndHiddenUnderLean(t *testing.T) {
	s, _ := contextForTaskTool(t)
	if tools.IsPinned("context_for_task") {
		t.Error("context_for_task must not be pinned")
	}
	if slices.Contains(tools.LeanToolNames(), "context_for_task") {
		t.Error("context_for_task must not be in the lean set")
	}
	s.mutate(func(v *sessionView) { v.tools.Profile = "lean" })
	if s.toolVisible("context_for_task") {
		t.Error("a lean client must not be advertised context_for_task")
	}
}

// The daemon seeds a connection's workspace pin from certain argument names.
// None of this tool's arguments may be one of them, or a scope glob in
// `within` (or a file in `files`) could move the pin of a connection that
// merely asked for a pack.
func TestContextForTask_NoArgumentSeedsAWorkspacePin(t *testing.T) {
	_, tool := contextForTaskTool(t)
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	// Control: the detector sees the spellings it exists to catch.
	for _, seeding := range []string{`{"paths":["/abs/x"]}`, `{"path":"/abs/x"}`, `{"uri":"file:///abs/x"}`, `{"workspace":"/abs/x"}`} {
		if seedPathFromArgs(json.RawMessage(seeding)) == "" {
			t.Fatalf("control: seedPathFromArgs ignored %s", seeding)
		}
	}
	for name := range schema.Properties {
		for _, value := range []any{[]string{"/abs/x", "/abs/y/**"}, "/abs/x"} {
			args, err := json.Marshal(map[string]any{name: value})
			if err != nil {
				t.Fatal(err)
			}
			if got := seedPathFromArgs(args); got != "" {
				t.Errorf("argument %q seeds the connection pin from %s (got %q)", name, args, got)
			}
		}
	}
}

// Through the real registration, a pinned connection resolves a relative file
// against its own pin and is refused a file outside it, and calling the tool
// leaves the pin where it was.
func TestContextForTask_PinnedWiringResolvesAgainstThePinAndRefusesOutsideIt(t *testing.T) {
	s, tool := contextForTaskTool(t)
	root, other := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(other, "x.go")
	if err := os.WriteFile(outside, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.mutate(func(v *sessionView) {
		v.acquiredRoot = root
		v.policy = s.buildPathPolicy(v)
	})

	raw, _ := json.Marshal(map[string]any{"files": []string{"main.go"}})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil || !strings.Contains(out, "file main.go") {
		t.Fatalf("a relative file in the pinned workspace should resolve: out=%q err=%v", out, err)
	}

	raw, _ = json.Marshal(map[string]any{"files": []string{outside}})
	if out, err := tool.Execute(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "different project") {
		t.Errorf("a file outside the pin must be refused by the boundary guard: out=%q err=%v", out, err)
	}
	if got := s.workspace(); got != root {
		t.Errorf("the pin moved from %q to %q", root, got)
	}
}

// Expansion reaches bodies the caller never named. Their withholding rests on the
// same sensitive-path decision write responses and history use, so the real
// registration must hand the tool that decision: without it every body the walk
// reaches would be delivered, whatever sensitive_globs says.
func TestContextForTask_ExpansionIsWiredToTheSensitivePathDecision(t *testing.T) {
	_, tool := contextForTaskTool(t)
	wired, ok := tool.(interface{ SensitiveWired() bool })
	if !ok {
		t.Fatalf("%T has no SensitiveWired", tool)
	}
	if !wired.SensitiveWired() {
		t.Error("context_for_task is registered without the sensitive-path decision (changeSensitive)")
	}
}

// The top symbol seeds are put to the language server through the connection's routing
// proxy, so the real registration must hand the tool one: without it every pack with a
// symbol seed would report LSP enrichment unavailable, however healthy the server.
func TestContextForTask_RefinementIsWiredToTheLanguageServer(t *testing.T) {
	_, tool := contextForTaskTool(t)
	wired, ok := tool.(interface{ LSPWired() bool })
	if !ok {
		t.Fatalf("%T has no LSPWired", tool)
	}
	if !wired.LSPWired() {
		t.Error("context_for_task is registered without a language server (the session's routing proxy)")
	}
}

// An unpinned connection that calls the tool with an absolute file is refused
// with a session_start handoff, and its pin is exactly as it was: the tool
// resolves against an existing pin and never creates or moves one.
func TestContextForTask_UnpinnedCallIsRefusedAndLeavesThePinAlone(t *testing.T) {
	s, tool := contextForTaskTool(t)
	file := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := s.workspace()
	raw, _ := json.Marshal(map[string]any{"files": []string{file}})
	out, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatalf("an unpinned call produced output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "session_start") {
		t.Errorf("the refusal does not hand off to session_start: %v", err)
	}
	if after := s.workspace(); after != before {
		t.Errorf("the pin changed from %q to %q", before, after)
	}
}
