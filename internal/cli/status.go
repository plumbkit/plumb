package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/tui"
)

var statusTheme string

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Open the sessions dashboard (TUI)",
	RunE: func(_ *cobra.Command, _ []string) error {
		if statusTheme != "" {
			theme, ok := tui.AvailableThemes[strings.ToLower(statusTheme)]
			if !ok {
				return fmt.Errorf("unknown theme %q — available: %s", statusTheme, strings.Join(tui.ThemeNames(), ", "))
			}
			tui.ActiveTheme = theme
		}
		tui.Version = Version
		return tui.Run()
	},
}

func init() {
	statusCmd.Flags().StringVar(
		&statusTheme,
		"theme",
		"",
		fmt.Sprintf("colour theme for the TUI (available: %s)", strings.Join(tui.ThemeNames(), ", ")),
	)
}
