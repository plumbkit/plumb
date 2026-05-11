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

var configShowWorkspace string

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show resolved configuration with source provenance",
	Long: `Print the resolved configuration as plumb actually sees it, with each
layer (defaults → global → project → env) labelled so you can tell where
each value came from. Pass --workspace to include a project-local
.plumb/config.toml in the merge.`,
	RunE: runConfigShow,
}

var configGeminiCmd = &cobra.Command{
	Use:   "gemini",
	Short: "Show Gemini CLI integration status",
	RunE:  runConfigGemini,
}

func init() {
	configShowCmd.Flags().StringVar(&configShowWorkspace, "workspace", "",
		"Workspace directory to merge .plumb/config.toml from (defaults to current dir)")
	configCmd.AddCommand(configPrintCmd, configShowCmd, configGeminiCmd)
}

func runConfigGemini(_ *cobra.Command, _ []string) error {
	path, err := GeminiConfigPath()
	if err != nil {
		return fmt.Errorf("locating Gemini CLI config: %w", err)
	}

	fmt.Printf("# Gemini CLI MCP configuration\n\n")
	fmt.Printf("Config path: %s\n", existsLabel(path))

	if _, err := os.Stat(path); err == nil {
		fmt.Println("\nPlumb can be registered in Gemini CLI using `plumb setup gemini`.")
	} else {
		fmt.Println("\nGemini CLI config not found. Ensure Gemini CLI is installed and has been run at least once.")
	}

	return nil
}

func runConfigShow(_ *cobra.Command, _ []string) error {
	ws := configShowWorkspace
	if ws == "" {
		if cwd, err := os.Getwd(); err == nil {
			ws = cwd
		}
	}

	defaultsCfg := config.Defaults()
	globalCfg, gerr := config.Load()
	if gerr != nil {
		return fmt.Errorf("loading global config: %w", gerr)
	}
	projectCfg, perr := config.LoadProject(globalCfg, ws)
	if perr != nil {
		return fmt.Errorf("loading project config: %w", perr)
	}

	globalPath := config.GlobalConfigPath()
	projectPath := config.ProjectConfigPath(ws)
	geminiPath, _ := GeminiConfigPath()

	fmt.Printf("# plumb resolved configuration\n\n")
	fmt.Printf("# global config:   %s\n", existsLabel(globalPath))
	fmt.Printf("# project config:  %s\n", existsLabel(projectPath))
	fmt.Printf("# gemini config:   %s\n", existsLabel(geminiPath))
	fmt.Printf("# workspace:       %s\n\n", ws)

	fmt.Printf("[edits]\n")
	fmt.Printf("strict                 = %v   # %s\n",
		projectCfg.Edits.Strict, sourceFor("strict", defaultsCfg.Edits.Strict, globalCfg.Edits.Strict, projectCfg.Edits.Strict))
	fmt.Printf("rate_limit_per_minute  = %d   # %s\n",
		projectCfg.Edits.RateLimitPerMinute,
		sourceFor("rate_limit_per_minute",
			defaultsCfg.Edits.RateLimitPerMinute,
			globalCfg.Edits.RateLimitPerMinute,
			projectCfg.Edits.RateLimitPerMinute))

	fmt.Printf("\n[cache]\n")
	fmt.Printf("ttl       = %s\n", projectCfg.Cache.TTL.Duration)
	fmt.Printf("max_size  = %d\n", projectCfg.Cache.MaxSize)

	fmt.Printf("\nlog_level = %q\n", projectCfg.LogLevel)
	if projectCfg.LogFile != "" {
		fmt.Printf("log_file  = %q\n", projectCfg.LogFile)
	}

	fmt.Printf("\n# Use `plumb config print` for the full TOML dump.\n")
	return nil
}

func existsLabel(path string) string {
	if path == "" {
		return "(none)"
	}
	if _, err := os.Stat(path); err == nil {
		return path + "  (exists)"
	}
	return path + "  (not found)"
}

// sourceFor returns a short label naming the layer that supplied the
// current value. Comparison is order-sensitive: env > project > global > default.
func sourceFor(field string, def, global, final any) string {
	if v := envForField(field); v != "" {
		return fmt.Sprintf("from env (%s=%s)", envVarForField(field), v)
	}
	switch {
	case fmt.Sprintf("%v", final) != fmt.Sprintf("%v", global):
		return "from project config"
	case fmt.Sprintf("%v", global) != fmt.Sprintf("%v", def):
		return "from global config"
	default:
		return "default"
	}
}

func envForField(field string) string {
	return os.Getenv(envVarForField(field))
}

func envVarForField(field string) string {
	switch field {
	case "strict":
		return "PLUMB_STRICT_EDITS"
	case "rate_limit_per_minute":
		return "PLUMB_WRITE_RATE_LIMIT"
	}
	return ""
}
