package tools

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

func TestIsKimiCode(t *testing.T) {
	cases := []struct {
		name   string
		client func() string
		want   bool
	}{
		{"exact", func() string { return "kimi-code" }, true},
		{"versioned", func() string { return "kimi-code/2.0" }, true},
		{"mixed case", func() string { return "Kimi-Code" }, true},
		// Bare "kimi" is a clientcaps.Lookup alias but NOT a guidance match: the
		// block is written for the Kimi Code CLI specifically, so a sibling
		// product would get wrong advice. See isKimiCode's doc comment.
		{"bare kimi is not a guidance match", func() string { return "kimi" }, false},
		{"prefix-only false positive", func() string { return "kimi-codegen" }, false},
		{"unrelated client", func() string { return "claude-code" }, false},
		{"empty", func() string { return "" }, false},
		{"nil accessor", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isKimiCode(c.client); got != c.want {
				t.Errorf("isKimiCode = %v, want %v", got, c.want)
			}
		})
	}
}

// kimiGuidance renders the Kimi Code guidance block with topology on or off.
func kimiGuidance(topologyOn bool) string {
	s := &SessionStart{
		clientNameFn: func() string { return "kimi-code" },
		toolProfile:  func() (string, int, string) { return "full", 0, "schema-discovery-only-client" },
	}
	if topologyOn {
		s.topo = func() *topology.Store { return &topology.Store{} }
	}
	var sb strings.Builder
	s.writeSessionGuidance(&sb)
	return sb.String()
}

// TestKimiCodeGuidance_LeanSetOnly is the load-bearing property. Kimi Code
// filters tools client-side (the enabledTools allowlist `plumb setup kimi-code
// --lean` writes), and plumb cannot see that filter — so the block must name
// ONLY lean tools, in both topology states, or it would point a --lean user at
// a tool their own config removed. The forbidden set is derived from
// nonLeanToolSet(), not a hand-picked sample, so a tool moving out of LeanTools
// fails this test instead of silently creating a broken pointer.
//
// Matching is plain substring, which also bans the bare English words that
// happen to be tool names (`version`). That is a feature, not a nuisance: the
// block is short, and a reader cannot tell a prose "version" from a tool
// reference any better than this test can.
func TestKimiCodeGuidance_LeanSetOnly(t *testing.T) {
	for _, topologyOn := range []bool{true, false} {
		out := kimiGuidance(topologyOn)
		if !strings.Contains(out, "## Tool guidance (Kimi Code)") {
			t.Fatalf("topology=%v: guidance block missing:\n%s", topologyOn, out)
		}
		for _, tl := range nonLeanToolSet() {
			if strings.Contains(out, tl.Name()) {
				t.Errorf("topology=%v: guidance names non-lean tool %q — a client-side allowlist would hide it:\n%s",
					topologyOn, tl.Name(), out)
			}
		}
	}
}

// TestKimiCodeGuidance_NamesTheToolsItSteersTo is the positive half: the block
// must actually cite the lean tools it claims to, and close with the --lean
// pointer that is the whole point of the Kimi support.
func TestKimiCodeGuidance_NamesTheToolsItSteersTo(t *testing.T) {
	out := kimiGuidance(true)

	citedTools := []string{
		"read_file", "edit_file", "write_file", "undo_edit", "transaction_apply",
		"topology_affected", "topology_search", "file_outline",
		"get_definition", "find_references", "workspace_symbols", "rename_symbol", "diagnostics",
		"run_task",
	}
	for _, name := range citedTools {
		if !IsLean(name) {
			t.Fatalf("fixture error: %q is asserted as cited but is not in LeanTools", name)
		}
		if !strings.Contains(out, name) {
			t.Errorf("guidance missing lean tool %q:\n%s", name, out)
		}
	}

	for _, phrase := range []string{"expected_mtime", "plumb setup kimi-code --lean"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("guidance missing %q:\n%s", phrase, out)
		}
	}
}

// TestKimiCodeGuidance_TopologyOffFallsBackToTheEnableHint pins the off-branch:
// with no topology store wired, the Map trio must not be advertised (those calls
// would fail) and the block points at enabling the index instead.
func TestKimiCodeGuidance_TopologyOffFallsBackToTheEnableHint(t *testing.T) {
	out := kimiGuidance(false)
	if strings.Contains(out, "topology_search") {
		t.Errorf("topology-off guidance must not steer to the index:\n%s", out)
	}
	if !strings.Contains(out, "[topology] enabled = true") {
		t.Errorf("topology-off guidance should point at enabling the index:\n%s", out)
	}
}

// TestKimiCodeGuidance_NoClaudeCodeHarnessErrors pins the deliberate choice NOT
// to reuse nativeEditLaneWarning: it quotes Claude Code's exact harness error
// strings, which a Kimi Code user will never see. Quoting them would be
// confident, specific, and wrong.
func TestKimiCodeGuidance_NoClaudeCodeHarnessErrors(t *testing.T) {
	out := kimiGuidance(true)
	for _, forbidden := range []string{
		"File has not been read yet",
		"File has been modified since read",
		"Claude Code",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("Kimi guidance must not carry the Claude Code harness wording %q:\n%s", forbidden, out)
		}
	}
}

// TestKimiCodeHasNoNativeEditConflict pins Kimi Code OUT of the native-edit
// conflict list. That list is confirmed-cases-only: it drives a warning naming
// two verbatim Claude Code harness errors, so a client is added only once a real
// session has been observed producing harness-side read-before-edit refusals
// (the "File has not been read yet" / "File has been modified since read"
// rejections) when a plumb read_file is followed by a native edit. No such
// evidence exists for Kimi Code; inferring it from "has native file tools" is
// exactly the inference this predicate refuses to make.
func TestKimiCodeHasNoNativeEditConflict(t *testing.T) {
	for _, client := range []string{"kimi-code", "kimi-code/0.0.0", "kimi"} {
		if clientHasNativeEditConflict(func() string { return client }) {
			t.Errorf("clientHasNativeEditConflict(%q) = true, want false — promote only on observed evidence", client)
		}
	}
}
