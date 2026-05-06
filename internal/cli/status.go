package cli

import (
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show connected LSPs, cache stats, and recent MCP calls (TUI)",
	RunE: func(_ *cobra.Command, _ []string) error {
		// TODO(Step 8): launch Bubble Tea v2 status TUI.
		return nil
	},
}
