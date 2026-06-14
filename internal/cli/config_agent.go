package cli

// config_agent.go — `plumb config unset` (the one-step revert for an
// agent-written value) and the provenance footer `plumb config show` prints so
// agent-written keys are visible and auditable.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tui"
)

var configUnsetWorkspace string

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Remove a project-config key (reverts an agent-written value to inherited)",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigUnset,
}

func init() {
	configUnsetCmd.Flags().StringVar(&configUnsetWorkspace, "workspace", "",
		"Workspace whose .plumb/config.toml to edit (defaults to current dir)")
	configCmd.AddCommand(configUnsetCmd)
}

func runConfigUnset(_ *cobra.Command, args []string) error {
	key := args[0]
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
	}
	root, err := resolveCLIWorkspace(configUnsetWorkspace, cfg)
	if err != nil {
		return err
	}
	if err := config.UnsetProjectValue(root, strings.Split(key, ".")); err != nil {
		return fmt.Errorf("unsetting %s: %w", key, err)
	}
	if err := config.DropProvenance(root, key); err != nil {
		return fmt.Errorf("dropping provenance for %s: %w", key, err)
	}
	// Best-effort: a running daemon re-applies project config so the revert is
	// live at once; otherwise the per-session watcher picks it up within 30s.
	_, _ = dialDaemonCtrl("reload-project " + root)
	fmt.Printf("unset %s in %s\n", key, root)
	return nil
}

// printAgentProvenance lists the keys the agent wrote in this workspace, so an
// agent-written value is never silent. No-op when the sidecar is absent.
func printAgentProvenance(workspace string) {
	prov, err := config.LoadProvenance(workspace)
	if err != nil || len(prov) == 0 {
		return
	}
	keys := make([]string, 0, len(prov))
	for k := range prov {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("Agent-written keys (%s)\n", tui.WarnStyle.Render("provenance=agent"))
	t := configShowTableBase().Headers("Key", "When", "Session")
	for _, k := range keys {
		e := prov[k]
		t.Row(k, e.Timestamp.Format("2006-01-02 15:04"), e.SessionID)
	}
	fmt.Println(renderConfigShowTable(t))
	fmt.Println("Revert with: plumb config unset <key> --workspace " + workspace)
	fmt.Println()
}
