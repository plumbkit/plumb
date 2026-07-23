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
	"sort"
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
	if _, fromProject := taskProvenance(root, lang, slot); fromProject {
		cmds, cerr := config.ProjectTaskCommands(root)
		if cerr != nil {
			return cerr
		}
		if !config.NewTrustStore().IsTrustedForTasks(root, cmds) {
			return fmt.Errorf("the %s command for %s comes from this project's .plumb/config.toml and is not trusted "+
				"(or the project's task commands changed since `plumb trust` was last run); run `plumb trust` in %s first", slot, lang, root)
		}
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
	cmds, err := config.ProjectTaskCommands(root)
	if err != nil {
		return err
	}
	// Informed consent: show every project-supplied command trust is about to
	// bind to — config.ProjectTaskCommands, the same set SetTrustedForTasks
	// hashes below, covering every language the project configures, not just
	// the one currently detected. Trust is enforced against the command at run
	// time, so surfacing it here is the user's chance to spot a hostile argv
	// (e.g. an interpreter invocation) a project shipped — and re-running
	// `plumb trust` after a command changes re-confirms the current set.
	printTrustedTaskCommands(root, cmds)
	if err := config.NewTrustStore().SetTrustedForTasks(root, cmds); err != nil {
		return err
	}
	// Trust is bound to the exact command set above: changing any task command
	// invalidates it and re-prompts. A trust.json upgraded from the old boolean
	// format re-confirms here once.
	fmt.Printf("trusted project task commands for %s (trust is bound to these commands; changing them requires re-running `plumb trust`)\n", root)
	return nil
}

// printTrustedTaskCommands lists every project-supplied task command in cmds
// (config.ProjectTaskCommands(root) — the exact set the trust hash binds to),
// grouped by language, so `plumb trust` is informed consent over exactly what
// is trusted rather than a blind grant limited to the currently-detected
// language. Default/global commands are never included in cmds: they run
// without trust. A no-op when cmds is empty (nothing to trust, nothing to
// show).
func printTrustedTaskCommands(root string, cmds []config.TaskCommandSpec) {
	if len(cmds) == 0 {
		return
	}
	byLang := make(map[string]map[string]string, len(cmds))
	for _, c := range cmds {
		if byLang[c.Lang] == nil {
			byLang[c.Lang] = make(map[string]string)
		}
		byLang[c.Lang][c.Slot] = c.Command
	}
	langs := make([]string, 0, len(byLang))
	for lang := range byLang {
		langs = append(langs, lang)
	}
	sort.Strings(langs)

	fmt.Printf("about to trust these project-supplied task commands in %s:\n", root)
	for _, lang := range langs {
		fmt.Printf("  [%s]\n", lang)
		printLangTaskCommands(byLang[lang])
	}
}

// printLangTaskCommands prints one language's slot -> command entries, known
// TaskSlots first in their canonical order, then any other slot name the
// project config supplied (sorted) — so a typo'd or future slot name is still
// disclosed, since it is still part of what gets trusted.
func printLangTaskCommands(slots map[string]string) {
	for _, slot := range orderedTaskSlotNames(slots) {
		cmd := slots[slot]
		display := cmd
		if slot == "verify" {
			display = "(composite: build then test)"
		}
		fmt.Printf("    %-7s %s\n", slot, display)
		if argv, perr := config.ParseTaskCommand(cmd); perr == nil && config.FlagsInlineInterpreter(argv) {
			fmt.Printf("    %-7s !! WARNING: this runs an interpreter with inline code (%s) — arbitrary code execution by design; trust only if you wrote it\n", "", argv[0])
		}
	}
}

// orderedTaskSlotNames returns slots' keys with the recognised config.TaskSlots
// first (in their canonical order), followed by any other key present (sorted),
// so every entry ends up shown exactly once.
func orderedTaskSlotNames(slots map[string]string) []string {
	names := make([]string, 0, len(slots))
	known := make(map[string]bool, len(config.TaskSlots))
	for _, s := range config.TaskSlots {
		known[s] = true
		if _, ok := slots[s]; ok {
			names = append(names, s)
		}
	}
	var extra []string
	for s := range slots {
		if !known[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	return append(names, extra...)
}
