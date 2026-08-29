package cli

// tasks_cmd.go — the `plumb build|lint|test|e2e|verify` task commands. They
// resolve the workspace's configured [tasks.<lang>] command, apply the same
// per-workspace trust gate the run_task MCP tool uses, and stream the command's
// output (no cap — a CLI run is interactive, unlike the tool). `plumb trust`
// itself lives in trust_cmd.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
)

var taskCmds = func() []*cobra.Command {
	out := make([]*cobra.Command, 0, len(config.TaskSlots)+1)
	for _, slot := range config.TaskSlots {
		out = append(out, &cobra.Command{
			Use:   slot + " [target]",
			Short: "Run the configured " + slot + " command for this workspace's language",
			Args:  cobra.MaximumNArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return runTaskCLI(slot, args) },
		})
	}
	return append(out, taskSlotCmd())
}()

// taskSlotCmd is the CLI path to a slot the PROJECT named, which the five fixed
// verbs above cannot reach.
//
// Those are registered at package init, from the built-in list, long before any
// workspace is resolved — so a project-defined slot cannot get a verb of its own
// without resolving the workspace during command registration. One generic verb
// avoids that entirely: `plumb task check` runs what `run_task {slot: "check"}`
// runs, through the same resolver and the same trust gate. It also works for the
// built-ins, so there is one spelling that always works.
func taskSlotCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "task [slot] [target]",
		Short: "Run a task slot by name, including a slot this project defined",
		Long: "Run a stored [tasks.<lang>] command by slot name.\n\n" +
			"The five built-in slots have verbs of their own (plumb build, plumb test, …); this is how a\n" +
			"slot the project defined is run, since those verbs are fixed at build time. Run it with no\n" +
			"arguments to list the slots configured for this workspace.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return listTaskSlots(cmd)
			}
			if !config.ValidTaskSlotName(args[0]) {
				return fmt.Errorf("%q is not a valid slot name "+
					"(lowercase letter first, then letters, digits, _ or -, max 32 characters)", args[0])
			}
			return runTaskCLI(args[0], args[1:])
		},
	}
}

// listTaskSlots reports the slots that actually have a command here, so a bare
// `plumb task` teaches the workspace's vocabulary rather than printing usage.
func listTaskSlots(cmd *cobra.Command) error {
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
	return writeTaskSlotListing(cmd.OutOrStdout(), root, lang, projectCfg.Tasks[lang])
}

// writeTaskSlotListing renders the runnable slots for one language. It is split
// from listTaskSlots so the rendering can be tested without ambient workspace
// detection: under `make`, GOTMPDIR sits INSIDE the checkout, so a t.TempDir()
// fixture is nested in plumb's own Go repository — and package.json is a weak
// root marker that correctly loses to the enclosing go.mod, so a fixture built
// that way resolves as Go on CI and as TypeScript locally.
func writeTaskSlotListing(w io.Writer, root, lang string, tc config.TasksConfig) error {
	slots := config.ConfiguredSlotNames(tc)
	have := make([]string, 0, len(slots))
	for _, slot := range slots {
		if steps, err := buildTaskSteps(tc, lang, slot, ""); err == nil && len(steps) > 0 {
			have = append(have, slot)
		}
	}
	if len(have) == 0 {
		return fmt.Errorf("no task commands configured for %s in %s; set them under [tasks.%s]", lang, root, lang)
	}
	fmt.Fprintf(w, "task slots for %s in %s:\n", lang, root)
	for _, slot := range have {
		fmt.Fprintf(w, "  %-12s %s\n", slot, renderedTaskCommand(tc, lang, slot))
	}
	return nil
}

// renderedTaskCommand renders what run_task WILL run for a slot, not what the
// config file happens to store.
//
// Two ways those differ. A composite stores no command of its own, so printing
// the raw string leaves the slot looking unconfigured in the very listing that
// exists to say it is runnable. And a stored command that reconciliation
// restores a {target} placeholder into is scopable, while the raw string plainly
// is not — a reader of `test  go test ./...` concludes a scoped run will be
// refused, which is the exact wrong belief this card exists to correct, printed
// by plumb itself. It is the doctrine configuredSlots already states: a report
// that contradicts the tool it describes is worse than no report.
func renderedTaskCommand(tc config.TasksConfig, lang, slot string) string {
	stored := strings.TrimSpace(tc.Get(slot))
	if subs, ok := compositeSubSlots(slot); ok {
		return "(composite: " + strings.Join(subs, ", then ") + ")"
	}
	raw, rerr := config.ParseTaskCommand(stored)
	tmpl, terr := taskArgvTemplate(tc, lang, slot)
	if rerr != nil || terr != nil || tmpl == nil || slices.Equal(raw, tmpl) {
		return stored
	}
	return fmt.Sprintf("%s   (placeholder restored from plumb's default; your config spells it %q)",
		strings.Join(tmpl, " "), stored)
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
	steps, err := taskStepsOrRefusal(root, projectCfg.Tasks[lang], lang, slot, target)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("no %s command configured for %s (configured slots: %s); "+
			"set one under [tasks.%s] in .plumb/config.toml",
			slot, lang, strings.Join(configuredSlots(projectCfg.Tasks[lang], lang), ", "), lang)
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
	for _, note := range taskNotes(projectCfg.Tasks[lang], lang, slot, target) {
		fmt.Fprintf(os.Stderr, "note: %s\n", note)
	}
	return runTaskSteps(root, slot, steps)
}

func runTaskSteps(root, slot string, steps [][]string) error {
	for i, argv := range steps {
		fmt.Fprintf(os.Stderr, "$ %s\n", strings.Join(argv, " "))
		if err := streamArgv(root, argv); err != nil {
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
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
