package cli

import (
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/tui"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Open the sessions dashboard (TUI)",
	RunE: func(_ *cobra.Command, _ []string) error {
		tui.Version = Version
		return tui.Run()
	},
}
