package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestSelftestPrompt_Metadata(t *testing.T) {
	p := NewSelftestPrompt(nil)
	if p.Name() != "selftest" {
		t.Errorf("Name() = %q, want %q", p.Name(), "selftest")
	}
	if p.Description() == "" {
		t.Error("Description() is empty")
	}
	args := p.Arguments()
	if len(args) != 1 || args[0].Name != "workspace" {
		t.Errorf("Arguments() = %+v, want a single workspace arg", args)
	}
}

func TestSelftestPrompt_Expand(t *testing.T) {
	p := NewSelftestPrompt(func() string { return "/tmp/ws" })
	msgs, err := p.Expand(context.Background(), nil)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("want one user message, got %+v", msgs)
	}
	text := msgs[0].Content.Text
	for _, want := range []string{
		"plumb self-test",
		"Preflight",
		"Sandbox setup",
		"Cleanup (MANDATORY",
		"PASS / FAIL / SKIP",
		`session_start({"workspace": "/tmp/ws"})`, // ws threaded into the call
	} {
		if !strings.Contains(text, want) {
			t.Errorf("playbook missing %q", want)
		}
	}
}

// TestSelftestPrompt_CoversEveryTool is the in-package half of the anti-rot
// guard: every tool in the canonical list must be named verbatim in the
// playbook, so the checklist cannot silently drop a tool. The integration
// harness checks the other half — that the canonical list equals the live
// tools/list.
func TestSelftestPrompt_CoversEveryTool(t *testing.T) {
	msgs, err := NewSelftestPrompt(nil).Expand(context.Background(), nil)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	text := msgs[0].Content.Text
	for _, name := range selftestToolNames() {
		if !strings.Contains(text, name) {
			t.Errorf("tool %q is in the coverage list but absent from the playbook", name)
		}
	}
}

func TestSelftestToolNames_NoDuplicates(t *testing.T) {
	seen := make(map[string]bool)
	for _, name := range selftestToolNames() {
		if seen[name] {
			t.Errorf("duplicate tool name in coverage list: %q", name)
		}
		seen[name] = true
	}
	if len(seen) == 0 {
		t.Fatal("coverage list is empty")
	}
}

func TestSelftestToolNames_ReturnsCopy(t *testing.T) {
	got := SelftestToolNames()
	if len(got) == 0 {
		t.Fatal("SelftestToolNames returned nothing")
	}
	got[0] = "mutated"
	if SelftestToolNames()[0] == "mutated" {
		t.Error("SelftestToolNames leaks its backing slice — callers can mutate the canonical list")
	}
}
