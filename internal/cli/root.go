// Package cli wires plumb's Cobra subcommands.
package cli

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
)

var logLevelFlag string

var rootCmd = &cobra.Command{
	Use:           "plumb",
	Short:         "MCP server exposing LSP capabilities to LLMs",
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		return setupLogging(logLevelFlag)
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&logLevelFlag, "log-level", "info", "log level (debug, info, warn, error)")
	rootCmd.AddCommand(serveCmd, statusCmd, setupCmd, versionCmd, configCmd)
}

// Execute runs the root command and returns any error.
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return err
	}
	return nil
}

func setupLogging(level string) error {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("invalid log level %q: %w", level, err)
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(h))
	return nil
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	RunE: func(_ *cobra.Command, _ []string) error {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			fmt.Println("plumb (version unknown)")
			return nil
		}
		version := info.Main.Version
		if version == "" || version == "(devel)" {
			version = "dev"
		}
		fmt.Printf("plumb %s (%s)\n", version, info.GoVersion)
		return nil
	},
}
