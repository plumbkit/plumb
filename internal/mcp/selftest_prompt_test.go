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

// selftestGroups is the coverage list as the playbook actually consumes it —
// one named group per roster line — so a guard can ask about the group a tool
// arrived in, which the flat selftestToolNames() cannot express.
func selftestGroups() []struct {
	name  string
	tools []string
} {
	return []struct {
		name  string
		tools []string
	}{
		{"bootstrap", selftestBootstrap},
		{"LSP queries", selftestLSPQuery},
		{"reads", selftestReads},
		{"git (read-only)", selftestGitRead},
		{"topology", selftestTopology},
		{"memory (read)", selftestMemoryRead},
		{"filesystem writes", selftestFSWrite},
		{"memory (write)", selftestMemoryWrite},
		{"session", selftestSession},
		{"collab", selftestCollab},
		{"tasks, commands & config", selftestTasksConfig},
		{"symbol edits", selftestSymbolEdit},
		{"harness-only", selftestHarnessOnly},
	}
}

// TestSelftestPrompt_CoversEveryTool is the in-package half of the anti-rot
// guard. The integration harness checks the other half — that the canonical
// list equals the live tools/list.
//
// It used to assert only `strings.Contains(playbook, name)` for each name in
// selftestToolNames(), which is close to vacuous: every name is rendered into a
// roster line by toolList() FROM THAT SAME LIST, so the check was reading back
// its own input. A tool could be added to the coverage list, appear in a comma
// separated roster, and the playbook could say nothing whatsoever about what to
// do with it — and this test would still pass, while claiming the checklist
// cannot silently drop a tool.
//
// What is asserted now is the property the name promises: the playbook must
// carry INSTRUCTION for every group, not an inventory of it.
//
//   - Each group's roster is rendered verbatim, so the coverage list demonstrably
//     drives the playbook rather than its names coincidentally occurring in prose.
//   - The bullet carrying that roster, with the generated roster text removed,
//     must still hold hand-written instruction. That residue is the only part of
//     the line a human wrote, so it is the only part that can tell an agent how
//     to exercise the tools — and the only part a generated check cannot fake.
//
// The floor is deliberately low (a short clause clears it). It is a bare-list
// detector, not a prose-quality bar: the failure it exists to catch is a new
// group pasted in as names alone, which is exactly what "assert against the
// instruction text" means here.
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

	const minInstruction = 40
	for _, g := range selftestGroups() {
		roster := toolList(g.tools)
		if !strings.Contains(text, roster) {
			t.Errorf("group %q: its roster is not rendered in the playbook — the group is in the "+
				"coverage list but no section lists it, so the tools are counted as covered and never driven",
				g.name)
			continue
		}
		instruction := strings.TrimSpace(selftestInstructionFor(text, roster))
		if len(instruction) < minInstruction {
			t.Errorf("group %q: its roster bullet carries %d bytes of instruction (%q), under the %d-byte "+
				"floor — the playbook lists these tools without telling the agent what to do with them, so "+
				"they are inventory, not coverage", g.name, len(instruction), instruction, minInstruction)
		}
	}
}

// selftestInstructionFor returns the hand-written remainder of the block that
// carries roster — the enclosing markdown bullet, or the enclosing paragraph
// when the roster is not in a bullet at all (Preflight closes a paragraph with
// "This step covers …") — with the generated roster text removed.
//
// The block is bounded on both sides by a blank line, a heading, or another
// bullet, so one group's prose is never credited to another. The bullet marker
// and any bold label are stripped too: "- **Memory (read):**" says which group
// this is, not what to do with it.
func selftestInstructionFor(text, roster string) string {
	i := strings.Index(text, roster)
	if i < 0 {
		return ""
	}
	lines := strings.Split(text, "\n")
	at := strings.Count(text[:i], "\n") // the line the roster sits on

	isBoundary := func(s string) bool {
		return strings.TrimSpace(s) == "" || strings.HasPrefix(s, "#")
	}
	start := at
	for start > 0 && !strings.HasPrefix(lines[start], "- ") &&
		!isBoundary(lines[start-1]) && !strings.HasPrefix(lines[start-1], "- ") {
		start--
	}
	end := at
	for end+1 < len(lines) && !isBoundary(lines[end+1]) && !strings.HasPrefix(lines[end+1], "- ") {
		end++
	}

	out := strings.TrimSpace(strings.ReplaceAll(strings.Join(lines[start:end+1], " "), roster, ""))
	out = strings.TrimPrefix(out, "- ")
	if strings.HasPrefix(out, "**") {
		if label := strings.Index(out[2:], "**"); label >= 0 {
			out = out[2+label+2:]
		}
	}
	return strings.TrimSpace(out)
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
