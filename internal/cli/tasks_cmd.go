package cli

// tasks_cmd.go — the `plumb build|lint|test|e2e|verify` task commands and
// `plumb trust`. They resolve the workspace's configured [tasks.<lang>] command,
// apply the same per-workspace trust gate the run_task MCP tool uses, and stream
// the command's output (no cap — a CLI run is interactive, unlike the tool).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
)

var taskCmds = func() []*cobra.Command {
	out := make([]*cobra.Command, 0, len(config.TaskSlots))
	for _, slot := range config.TaskSlots {
		slot := slot
		out = append(out, &cobra.Command{
			Use:   slot + " [target]",
			Short: "Run the configured " + slot + " command for this workspace's language",
			Args:  cobra.MaximumNArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return runTaskCLI(slot, args) },
		})
	}
	return out
}()

var trustCmd = &cobra.Command{
	Use:   "trust [directory]",
	Short: "Trust this workspace's project-supplied task commands (.plumb/config.toml)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runTrust,
}

// resolveTaskWorkspace resolves the workspace root and its primary language for
// a CLI task command.
func resolveTaskWorkspace(start string) (root, lang string, cfg config.Config, err error) {
	cfg, lerr := config.Load()
	if lerr != nil {
		cfg = config.Defaults()
	}
	root, err = resolveCLIWorkspace(start, cfg)
	if err != nil {
		return "", "", cfg, err
	}
	_, lang, _ = newWorkspacePool(context.Background(), cfg).Detect(root)
	return root, lang, cfg, nil
}

func runTaskCLI(slot string, args []string) error {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	root, lang, cfg, err := resolveTaskWorkspace("")
	if err != nil {
		return err
	}
	if lang == "" || lang == "none" {
		return fmt.Errorf("no language detected for %s; configure [tasks.<lang>] in your config", root)
	}
	projectCfg, err := config.LoadProject(cfg, root)
	if err != nil {
		return err
	}
	steps, err := buildTaskSteps(projectCfg.Tasks[lang], slot, target)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("no %s command configured for %s", slot, lang)
	}
	if _, fromProject := taskProvenance(root, lang, slot); fromProject && !config.NewTrustStore().IsTrusted(root) {
		return fmt.Errorf("the %s command for %s comes from this project's .plumb/config.toml and is not trusted; run `plumb trust` in %s first", slot, lang, root)
	}
	return runTaskSteps(root, slot, steps)
}

func runTaskSteps(root, slot string, steps [][]string) error {
	for i, argv := range steps {
		fmt.Fprintf(os.Stderr, "$ %s\n", strings.Join(argv, " "))
		if err := streamArgv(root, argv); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return fmt.Errorf("%s: step %d/%d failed (exit %d)", slot, i+1, len(steps), ee.ExitCode())
			}
			return fmt.Errorf("%s: %w", slot, err)
		}
	}
	fmt.Fprintln(os.Stderr, "ok")
	return nil
}

// streamArgv runs argv in dir with the terminal's stdio attached (no shell, no
// output cap — a CLI run is interactive).
func streamArgv(dir string, argv []string) error {
	// G204: the command is the user's own configured task; trust-gated above.
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // user-configured, trust-gated task command
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runTrust(_ *cobra.Command, args []string) error {
	start := ""
	if len(args) > 0 {
		start = args[0]
	}
	root, _, _, err := resolveTaskWorkspace(start)
	if err != nil {
		return err
	}
	if err := config.NewTrustStore().SetTrusted(root, true); err != nil {
		return err
	}
	fmt.Printf("trusted project task commands for %s\n", root)
	return nil
}
