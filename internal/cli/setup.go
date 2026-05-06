package cli

import (
	"github.com/spf13/cobra"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure plumb with external tools",
}

var setupClaudeDesktopCmd = &cobra.Command{
	Use:   "claude-desktop",
	Short: "Register plumb as an MCP server in Claude Desktop's config",
	RunE: func(_ *cobra.Command, _ []string) error {
		// TODO(Step 8): detect OS, locate claude_desktop_config.json, merge non-destructively.
		return nil
	},
}

func init() {
	setupCmd.AddCommand(setupClaudeDesktopCmd)
}
