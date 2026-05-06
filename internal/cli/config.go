package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/config"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect plumb configuration",
}

var configPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print the resolved configuration as TOML",
	RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		return config.Print(cfg, os.Stdout)
	},
}

func init() {
	configCmd.AddCommand(configPrintCmd)
}
