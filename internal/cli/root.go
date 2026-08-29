// Package cli wires plumb's Cobra subcommands.
package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tui"
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

// applyConfiguredTheme switches tui's package styles to the theme saved in the
// user's [ui] config, if one resolves. The dashboard has always launched this
// way; every other command renders through the same styles (the logo, ●
// headings, ┊ rows, status colours), so without this the whole CLI stayed on
// the default theme while the TUI followed the saved one. A config that fails
// to load or names an unknown theme keeps the default — a broken config must
// not take the CLI's colours down with it.
func applyConfiguredTheme() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	if t, ok := tui.AvailableThemes[cfg.UI.Theme]; ok {
		tui.ActiveTheme = t
		tui.ActiveThemeName = cfg.UI.Theme
		tui.RebuildStyles()
	}
}

func init() {
	// Print the logo banner before every command (logo, one blank line, then the
	// command's own output). Commands tagged annoSkipLogo opt out: the bare TUI
	// launch and the stdio-protocol commands (serve, daemon), where a banner on
	// stdout would corrupt the alt-screen or the MCP wire. printLogoIfNeeded is
	// idempotent, so the help/error paths never double-print.
	rootCmd.Annotations = map[string]string{annoSkipLogo: "true"}
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		applyConfiguredTheme()
		if suppressLogo(cmd) {
			return
		}
		printLogoIfNeeded(os.Stdout)
	}

	rootCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		// The help path skips PersistentPreRun, so the theme and the logo
		// banner need applying here too.
		applyConfiguredTheme()
		PrintLogo()

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

	rootCmd.AddCommand(serveCmd, daemonCmd, stopCmd, restartCmd, initCmd, setupCmd, skillsCmd, hooksCmd, versionCmd, configCmd, sessionsCmd, mailCmd, statsCmd, diagnosticsCmd, doctorCmd, logLevelCmd, enableLSPCmd, debugCmd, webCmd)
	rootCmd.AddCommand(trustCmd)
	rootCmd.AddCommand(taskCmds...)
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
// silentExitError is returned by commands that already printed their own
// failure summary and only need a non-zero exit code — Execute must not
// render a duplicate diagnostic for these.
type silentExitError struct{}

func (silentExitError) Error() string { return "" }

func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		if _, ok := errors.AsType[silentExitError](err); !ok {
			// The theme is applied in PersistentPreRun, which an unknown command
			// never reaches — cobra fails during command resolution. Without this
			// the banner and the diagnostic render in the default palette while
			// every other path renders in the user's, so an error looks like a
			// different program. Applying it here is idempotent.
			applyConfiguredTheme()
			printLogoIfNeeded(os.Stderr)
			printCLIDiagnostic(os.Stderr, cliDiagnostic{
				Kind:        "error",
				Title:       "error",
				Body:        err.Error(),
				Suggestions: diagnosticSuggestions(err),
			})
		}
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
