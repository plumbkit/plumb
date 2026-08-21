package tools

import (
	"sort"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// TestGuidanceNamesPinnedToolsOnly is the fix for the contradiction the card
// (PLAN-355) describes: session_start told a Claude Code agent to "start with
// workspace_search" while workspace_search's schema was deferred behind a
// ToolSearch round-trip, because the pin set used to be derived from
// tools.LeanTools rather than the tools Claude Code actually pins.
//
// The invariant asserted here: in every line of Claude Code guidance, a named
// tool is either in tools.PinnedTools (so its schema is already in context) or
// the same line explicitly says "ToolSearch" (so the agent knows to load it
// first). A line naming a deferred tool with no such note is a broken pointer
// — the exact failure mode this card fixes.
func TestGuidanceNamesPinnedToolsOnly(t *testing.T) {
	names := make([]string, 0, len(leanToolSet())+len(nonLeanToolSet()))
	for _, tl := range append(leanToolSet(), nonLeanToolSet()...) {
		names = append(names, tl.Name())
	}
	// Longest names first, so e.g. insert_before_symbol is matched whole rather
	// than a shorter name being found as a substring of it first.
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })

	render := func(profile string, topologyOn bool) string {
		s := &SessionStart{
			clientNameFn: func() string { return "claude-code" },
			toolProfile:  func() (string, int, string) { return profile, 0, "" },
		}
		if topologyOn {
			s.topo = func() *topology.Store { return &topology.Store{} }
		}
		var sb strings.Builder
		s.writeClaudeCodeGuidance(&sb)
		return sb.String()
	}

	for _, profile := range []string{"full", "lean"} {
		for _, topologyOn := range []bool{true, false} {
			out := render(profile, topologyOn)
			for _, line := range strings.Split(out, "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				seen := map[string]bool{}
				for _, name := range names {
					if seen[name] || !strings.Contains(line, name) {
						continue
					}
					seen[name] = true
					if IsPinned(name) {
						continue
					}
					if !strings.Contains(line, "ToolSearch") {
						t.Errorf("guidance (profile=%s, topology=%v) names deferred tool %q with no ToolSearch note:\n%s",
							profile, topologyOn, name, line)
					}
				}
			}
		}
	}
}
