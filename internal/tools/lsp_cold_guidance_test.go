package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// Cold-LSP guidance must always name the tree-sitter/topology backup, so an
// agent never reads a cold or slow language server as a dead end.

func TestLSPTimeout_MessageNamesTopologyBackup(t *testing.T) {
	err := lspTimeout("get_definition", context.DeadlineExceeded)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "did not respond in time") {
		t.Errorf("lost the timeout wording: %q", msg)
	}
	if !strings.Contains(msg, "topology_search") || !strings.Contains(msg, "tree-sitter topology index") {
		t.Errorf("timeout guidance must name the tree-sitter/topology backup: %q", msg)
	}
}

func TestLSPTimeoutErr_MessageNamesTopologyBackup(t *testing.T) {
	err := lspTimeoutErr("workspace_symbols", 30*time.Second, context.DeadlineExceeded)
	msg := err.Error()
	if !strings.Contains(msg, "did not respond within 30s") {
		t.Errorf("lost the timeout wording: %q", msg)
	}
	if !strings.Contains(msg, "topology_search") || !strings.Contains(msg, "tree-sitter topology index") {
		t.Errorf("timeout guidance must name the tree-sitter/topology backup: %q", msg)
	}
}

func TestColdLSPWarmingErr_Warming(t *testing.T) {
	fn := func(string) (bool, time.Duration) { return true, 4 * time.Second }
	err := coldLSPWarmingErr("explain_symbol", fn, "file:///p/main.go")
	if err == nil {
		t.Fatal("expected a warming error")
	}
	msg := err.Error()
	for _, want := range []string{"explain_symbol", "still warming", "(~4s elapsed)", "tree-sitter", "daemon_info"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warming message missing %q: %q", want, msg)
		}
	}
}

func TestColdLSPWarmingErr_FallsThroughWhenNotWarming(t *testing.T) {
	ready := func(string) (bool, time.Duration) { return false, 0 }
	if err := coldLSPWarmingErr("explain_symbol", ready, "file:///p/main.go"); err != nil {
		t.Errorf("a ready server must fall through to the ordinary error flavour, got: %v", err)
	}
	if err := coldLSPWarmingErr("explain_symbol", nil, "file:///p/main.go"); err != nil {
		t.Errorf("an unwired probe must fall through to the ordinary error flavour, got: %v", err)
	}
}

func TestRenameLSPFailureHint_WarmingLeadsWithWarmupState(t *testing.T) {
	hint := renameLSPFailureHint("Foo", "Bar", false, true, 3*time.Second)
	if !strings.Contains(hint, "still warming (~3s elapsed)") {
		t.Errorf("warming hint must state the warm-up state: %q", hint)
	}
	if !strings.Contains(hint, "daemon_info") || !strings.Contains(hint, "no tree-sitter fallback") {
		t.Errorf("warming hint must point at daemon_info and say rename has no tree-sitter fallback: %q", hint)
	}
}

func TestRenameLSPFailureHint_NotWarmingOmitsWarmupState(t *testing.T) {
	hint := renameLSPFailureHint("Foo", "Bar", false, false, 0)
	if strings.Contains(hint, "warming") || strings.Contains(hint, "daemon_info") {
		t.Errorf("a ready-server failure must not mention warming: %q", hint)
	}
}

// The Claude Code client guidance must state the cold-LSP ladder: what works
// via tree-sitter while the server warms, and what needs a ready one.
func TestSessionGuidance_NamesColdLSPLadder(t *testing.T) {
	s := &SessionStart{
		clientNameFn: func() string { return "claude-code" },
		toolProfile:  func() (string, int, string) { return "full", 0, "" },
		topo:         func() *topology.Store { return &topology.Store{} },
	}
	var sb strings.Builder
	s.writeSessionGuidance(&sb)
	out := sb.String()
	if !strings.Contains(out, "Cold LSP") || !strings.Contains(out, "tree-sitter") || !strings.Contains(out, "move_symbol") {
		t.Errorf("guidance must state the cold-LSP ladder:\n%s", out)
	}
}

// Under the lean profile the symbol-edit and hierarchy tools are hidden from
// tools/list, so the cold-LSP ladder line must be suppressed with them.
func TestSessionGuidance_LeanProfileOmitsColdLSPLadder(t *testing.T) {
	s := &SessionStart{
		clientNameFn: func() string { return "claude-code" },
		toolProfile:  func() (string, int, string) { return "lean", 33, "verified-deferred-discovery" },
		topo:         func() *topology.Store { return &topology.Store{} },
	}
	var sb strings.Builder
	s.writeSessionGuidance(&sb)
	out := sb.String()
	if strings.Contains(out, "Cold LSP") {
		t.Errorf("lean guidance must not name hidden tools:\n%s", out)
	}
}
