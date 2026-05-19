// Package cli wires plumb's Cobra subcommands.
package cli

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/tui"
)

var rootCmd = &cobra.Command{
	Use:           "plumb",
	Short:         "MCP server exposing LSP capabilities to LLMs",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(_ *cobra.Command, _ []string) error {
		tui.Version = Version
		return tui.Run(daemonLogPath(), daemonCtrlSocketPath())
	},
}

func init() {
	rootCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		PrintLogo()
		tui.RebuildStyles()

		// Print Usage
		if cmd.UseLine() != "" {
			fmt.Println(tui.ItemStyle.Render("Usage:"))
			fmt.Println("  " + cmd.UseLine())
			if cmd.HasAvailableSubCommands() {
				fmt.Println("  " + cmd.CommandPath() + " [command]")
			}
			fmt.Println()
		}

		// Print Aliases
		if len(cmd.Aliases) > 0 {
			fmt.Println(tui.ItemStyle.Render("Aliases:"))
			fmt.Println("  " + cmd.NameAndAliases())
			fmt.Println()
		}

		// Print Available Commands
		if cmd.HasAvailableSubCommands() {
			fmt.Println(tui.ItemStyle.Render("Available Commands:"))
			nameWidth := availableCommandNameWidth(cmd)
			for _, c := range cmd.Commands() {
				if c.IsAvailableCommand() {
					name := fmt.Sprintf("  %-*s", nameWidth, c.Name())
					fmt.Printf("%s %s\n", tui.HintStyle.Bold(true).Render(name), tui.MutedStyle.Render(c.Short))
				}
			}
			fmt.Println()
		}

		// Print local non-persistent flags under "Flags:"
		if cmd.LocalNonPersistentFlags().HasAvailableFlags() {
			fmt.Println(tui.ItemStyle.Render("Flags:"))
			fmt.Println(tui.MutedStyle.Render(strings.TrimRight(cmd.LocalNonPersistentFlags().FlagUsages(), "\n")))
			fmt.Println()
		}

		// Print persistent flags under "Global Flags:". For the root command
		// these are its own persistent flags; for subcommands they are inherited.
		if !cmd.HasParent() {
			if cmd.PersistentFlags().HasAvailableFlags() {
				fmt.Println(tui.ItemStyle.Render("Global Flags:"))
				fmt.Println(tui.MutedStyle.Render(strings.TrimRight(cmd.PersistentFlags().FlagUsages(), "\n")))
				fmt.Println()
			}
		} else if cmd.HasAvailableInheritedFlags() {
			fmt.Println(tui.ItemStyle.Render("Global Flags:"))
			fmt.Println(tui.MutedStyle.Render(strings.TrimRight(cmd.InheritedFlags().FlagUsages(), "\n")))
			fmt.Println()
		}

		// Print Footer
		if cmd.HasAvailableSubCommands() {
			fmt.Println(tui.MutedStyle.Render(fmt.Sprintf("Use \"%s [command] --help\" for more information about a command.", cmd.CommandPath())))
		}
	})

	rootCmd.AddCommand(serveCmd, daemonCmd, stopCmd, initCmd, setupCmd, versionCmd, configCmd, sessionsCmd, statsCmd, diagnosticsCmd, doctorCmd, logLevelCmd)
}

func availableCommandNameWidth(cmd *cobra.Command) int {
	maxName := 0
	for _, c := range cmd.Commands() {
		if c.IsAvailableCommand() && len(c.Name()) > maxName {
			maxName = len(c.Name())
		}
	}
	return maxName + 1
}

// Execute runs the root command and returns any error.
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		printLogoIfNeeded(os.Stderr)
		printCLIDiagnostic(os.Stderr, cliDiagnostic{
			Kind:        "error",
			Title:       "error",
			Body:        err.Error(),
			Suggestions: diagnosticSuggestions(err),
		})
		return err
	}
	return nil
}

func diagnosticSuggestions(err error) []string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unknown command"):
		return []string{"plumb --help"}
	case strings.Contains(msg, "no workspace") || strings.Contains(msg, "could not resolve a project"):
		return []string{
			"plumb init",
			"plumb status --workspace /path/to/project",
		}
	default:
		return nil
	}
}

func setupLogging(level, format string) error {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("invalid log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: l}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	return nil
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	RunE: func(_ *cobra.Command, _ []string) error {
		PrintLogo()

		goVersion := "unknown"
		if info, ok := debug.ReadBuildInfo(); ok {
			goVersion = info.GoVersion
		}
		fmt.Printf("plumb %s (%s)\n", Version, goVersion)
		return nil
	},
}
