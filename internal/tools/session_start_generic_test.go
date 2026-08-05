package tools

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

const genericHeader = "## Tool guidance\n"

// genericGuidance renders the session guidance for clientName at the given
// profile and topology state.
func genericGuidance(clientName, profile string, topologyOn bool) string {
	s := &SessionStart{
		clientNameFn: func() string { return clientName },
		toolProfile:  func() (string, int, string) { return profile, 0, "test" },
	}
	if topologyOn {
		s.topo = func() *topology.Store { return &topology.Store{} }
	}
	var sb strings.Builder
	s.writeSessionGuidance(&sb)
	return sb.String()
}

// TestGenericGuidance_CoversEveryClientWithNoBespokeBlock is the point of the
// block. Eleven of plumb's fourteen setup targets used to fall through
// writeSessionGuidance's switch and receive nothing but the profile note — and
// they are exactly the clients that install no skills, so when the routing left
// the tool descriptions they were left with no steering channel at all.
//
// The negative half pins that the three clients WITH a bespoke block keep it:
// the generic block is a fallback, not an addition, and emitting both would pay
// for the same advice twice in every session.
func TestGenericGuidance_CoversEveryClientWithNoBespokeBlock(t *testing.T) {
	fallsThrough := []string{
		"codex", "gemini", "cursor", "augment", "qwen",
		"antigravity", "opencode", "crush", "goose", "hermes",
		"", "some-agent-nobody-has-heard-of",
	}
	for _, name := range fallsThrough {
		t.Run("generic/"+name, func(t *testing.T) {
			if out := genericGuidance(name, "full", true); !strings.Contains(out, genericHeader) {
				t.Errorf("client %q got no guidance block at all:\n%s", name, out)
			}
		})
	}

	bespoke := map[string]string{
		"claude-code": "## Tool guidance (Claude Code)",
		"claude-ai":   "## Tool guidance (Claude Desktop)",
		"kimi-code":   "## Tool guidance (Kimi Code)",
	}
	for name, header := range bespoke {
		t.Run("bespoke/"+name, func(t *testing.T) {
			out := genericGuidance(name, "full", true)
			if !strings.Contains(out, header) {
				t.Errorf("client %q lost its bespoke block:\n%s", name, out)
			}
			if strings.Contains(out, genericHeader) {
				t.Errorf("client %q got the generic block as well as its own:\n%s", name, out)
			}
		})
	}
}

// TestGenericGuidance_RestoresTheRoutingTheDRYPassRemoved is the positive half:
// the block must actually carry the routing that left the tool descriptions,
// not merely exist. Each name here was in a description's comparative-routing
// prose before the DRY pass.
func TestGenericGuidance_RestoresTheRoutingTheDRYPassRemoved(t *testing.T) {
	out := genericGuidance("codex", "full", true)
	for _, want := range []string{
		"workspace_search", "search_in_files", "get_definition", "find_references",
		"topology_affected", "read_file", "edit_file", "expected_mtime",
		"rename_symbol", "diagnostics", "run_task",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generic guidance does not name %q:\n%s", want, out)
		}
	}
}

// TestGenericGuidance_LeanSetOnly holds the same property the Kimi Code block
// does, for the same reason: guidance must never steer an agent at a tool its
// resolved profile hides from tools/list. The forbidden set is derived from
// nonLeanToolSet(), so a tool leaving LeanTools fails here rather than silently
// turning a line of guidance into a broken pointer.
func TestGenericGuidance_LeanSetOnly(t *testing.T) {
	for _, topologyOn := range []bool{true, false} {
		out := genericGuidance("codex", "lean", topologyOn)
		if !strings.Contains(out, genericHeader) {
			t.Fatalf("topology=%v: generic block missing:\n%s", topologyOn, out)
		}
		for _, tl := range nonLeanToolSet() {
			if strings.Contains(out, tl.Name()) {
				t.Errorf("topology=%v: generic guidance names non-lean tool %q:\n%s",
					topologyOn, tl.Name(), out)
			}
		}
	}
}

// TestGenericGuidance_TopologyOffDropsTheMap pins the off-branch: with no
// topology store the index tools must not be advertised (those calls would
// fail), and the block points at enabling the index instead.
func TestGenericGuidance_TopologyOffDropsTheMap(t *testing.T) {
	out := genericGuidance("gemini", "full", false)
	if !strings.Contains(out, "[topology] enabled = true") {
		t.Fatalf("topology-off block does not point at enabling the index:\n%s", out)
	}
	// The enable tip legitimately names topology_affected — it is describing what
	// turning the index on would buy. What must not happen is the ADVICE offering
	// an index tool as a move to make now, so the ban applies to everything above
	// the tip.
	advice := out[:strings.Index(out, "Tip:")]
	for _, banned := range []string{"topology_affected", "topology_search"} {
		if strings.Contains(advice, banned) {
			t.Errorf("topology is off but the advice offers %q:\n%s", banned, out)
		}
	}
	// The non-index routing must survive the topology-off branch — that is the
	// half the DRY pass removed, and it does not depend on the index.
	for _, want := range []string{"workspace_search", "get_definition", "expected_mtime", "run_task"} {
		if !strings.Contains(out, want) {
			t.Errorf("topology-off block dropped %q:\n%s", want, out)
		}
	}
}
