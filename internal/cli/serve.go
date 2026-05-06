package cli

import (
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the MCP server over stdio",
	RunE: func(_ *cobra.Command, _ []string) error {
		// TODO(Step 7): load config, start MCP server over stdio.
		return nil
	},
}
