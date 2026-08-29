package cli

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/stats"
)

// TestSemanticToolsAreRegistered guards stats.SemanticTools (health_semantic.go)
// against exactly the drift TestPinMembership guards tools.PinnedTools
// against: a rename or typo that leaves a stale name in the set, silently
// making that row of `plumb stats --health` always report zero calls. Probed
// against the REAL registered tool set (registerAllTools), not a hand-built
// stand-in — same discipline as buildTestConnSession's own doc comment.
func TestSemanticToolsAreRegistered(t *testing.T) {
	_, srv := buildTestConnSession(t)
	registered := map[string]bool{}
	for _, name := range srv.ToolNames() {
		registered[name] = true
	}
	for name := range stats.SemanticTools {
		if !registered[name] {
			t.Errorf("stats.SemanticTools[%q] is not in the registered tool set (registerAllTools) — "+
				"a rename or typo silently orphaned this entry", name)
		}
	}
}

// TestStatsHealth_RejectsIncompatibleFlags is the red-then-green fixture for
// review round 1's finding: --health combined with --since/--failures/--limit
// used to silently ignore the extra flag. checkHealthFlagCompat must refuse
// the combination, and must NOT refuse any of those flags used WITHOUT
// --health (the default view still honours them).
func TestStatsHealth_RejectsIncompatibleFlags(t *testing.T) {
	// A cobra.Command with its OWN flag definitions bound to LOCAL variables —
	// deliberately NOT statsCmd.Flags() (or AddFlagSet against it): cobra binds
	// a flag to the pointer it was declared with, so reusing statsCmd's real
	// flags here would parse straight into the package-level statsFlagSince/
	// statsFlagFailures/statsFlagLimit globals and leak into every other test
	// in this package that runs after this one (found the hard way: this test
	// originally broke TestRunStats_ShowsRows by leaving statsFlagFailures=true
	// behind it). checkHealthFlagCompat only needs cmd.Flags().Changed(name),
	// which works identically on an independent flag set.
	newCmd := func() *cobra.Command {
		c := &cobra.Command{Use: "stats"}
		c.Flags().Bool("health", false, "")
		c.Flags().String("since", "", "")
		c.Flags().Bool("failures", false, "")
		c.Flags().Int("limit", 20, "")
		return c
	}

	t.Run("health alone is fine", func(t *testing.T) {
		statsFlagHealth = true
		t.Cleanup(func() { statsFlagHealth = false })
		c := newCmd()
		if err := c.Flags().Parse([]string{"--health"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := checkHealthFlagCompat(c); err != nil {
			t.Errorf("checkHealthFlagCompat() = %v, want nil for --health alone", err)
		}
	})

	t.Run("since without health is fine", func(t *testing.T) {
		statsFlagHealth = false
		c := newCmd()
		if err := c.Flags().Parse([]string{"--since", "24h"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := checkHealthFlagCompat(c); err != nil {
			t.Errorf("checkHealthFlagCompat() = %v, want nil for --since without --health", err)
		}
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"health + since", []string{"--health", "--since", "24h"}},
		{"health + failures", []string{"--health", "--failures"}},
		{"health + limit", []string{"--health", "--limit", "5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statsFlagHealth = true
			t.Cleanup(func() { statsFlagHealth = false })
			c := newCmd()
			if err := c.Flags().Parse(tc.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			err := checkHealthFlagCompat(c)
			if err == nil {
				t.Fatalf("checkHealthFlagCompat() = nil, want an error for %v (silently-ignored flag)", tc.args)
			}
		})
	}
}
