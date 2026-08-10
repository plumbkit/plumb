package cli

// tasks_cmd.go — the `plumb build|lint|test|e2e|verify` task commands and
// `plumb trust`. They resolve the workspace's configured [tasks.<lang>] command,
// apply the same per-workspace trust gate the run_task MCP tool uses, and stream
// the command's output (no cap — a CLI run is interactive, unlike the tool).

import (
	"bufio"
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

// trustAssumeYes skips the confirmation prompt (`plumb trust --yes`). It is the
// only way to grant without a terminal — see confirmTrust.
var trustAssumeYes bool

var taskCmds = func() []*cobra.Command {
	out := make([]*cobra.Command, 0, len(config.TaskSlots))
	for _, slot := range config.TaskSlots {
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
	Short: "Trust everything this workspace's .plumb/config.toml supplies",
	Long: `Approve the settings a project's .plumb/config.toml supplies that plumb
otherwise ignores. This is ONE grant per workspace, and it covers all of them:

  · [tasks.<lang>]        build/lint/test/e2e commands run by run_task
  · [[command]]           the named command allow-list run by run_command
  · [commands]            the shell policy, including allow_shell
  · [lsp.<lang>]          command, args, env, initialization_options and the
                          root markers — the argv of a process plumb spawns
  · [git]                 the destructive and network tiers, and the
                          protected-branch list

A project config is an untrusted surface — cloning a repository ships one — so
none of that takes effect until you approve it here. Everything about to be
trusted is printed first, values and all: read it before answering for it.

Trust is bound to the exact content shown. Changing any of it (by hand, or by an
agent) invalidates that part of the grant, and plumb falls back to your global
config until you run this again.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTrust,
}

func init() {
	trustCmd.Flags().BoolVar(&trustAssumeYes, "yes", false,
		"Skip the confirmation prompt (required to grant trust non-interactively)")
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
	root, _, cfg, err := resolveTaskWorkspace(start)
	if err != nil {
		return err
	}
	cmds, err := config.ProjectTaskCommands(root)
	if err != nil {
		return err
	}
	spec, err := config.ProjectPolicySpecFor(root)
	if err != nil {
		return err
	}
	// Informed consent: show everything trust is about to bind to — the exact
	// sets SetTrustedForProject hashes below, covering every language the project
	// configures rather than just the one currently detected. Trust is enforced
	// against this content at run time, so surfacing it here is the user's chance
	// to spot a hostile argv a project shipped, and re-running `plumb trust`
	// after any of it changes re-confirms the current content.
	printTrustedTaskCommands(root, cmds)
	printTrustedPolicy(root, spec, cfg)
	if len(cmds) == 0 && spec.IsEmpty() {
		fmt.Printf("%s supplies nothing that needs trust; recording the grant anyway so a later addition re-prompts\n", root)
	} else if err := confirmTrust(root); err != nil {
		return err
	}
	if err := config.NewTrustStore().SetTrustedForProject(root, cmds, spec); err != nil {
		return err
	}
	// Trust is bound to the exact content above: changing any of it invalidates
	// that binding and re-prompts. A trust.json upgraded from the old boolean
	// format re-confirms here once.
	fmt.Printf("trusted %s\n", root)
	fmt.Println("  this grant covers the project's task commands, its [[command]] allow-list and [commands] shell policy,")
	fmt.Println("  its [lsp.<lang>] server command/args/env, and its [git] tier policy.")
	fmt.Println("  it is bound to the content shown above; changing any of it requires re-running `plumb trust`.")
	return nil
}

// policyDisclosureLimit caps the per-key listing. The key set is attacker-chosen
// — a repository can pad [git] with any number of junk keys, all captured by the
// deliberate whole-table extraction — so an uncapped listing is a way to push the
// lines that matter off the user's scrollback.
const policyDisclosureLimit = 40

// printTrustedPolicy lists every capability-granting key the project's
// .plumb/config.toml sets — the [git] safety tiers and the [lsp.<lang>] fields
// that decide which process the daemon spawns and with what — as `key = value`,
// then the warnings.
//
// The values, not just the keys, are the point. A user approving a `command`
// deserves to see the argv before approving it, since after this the daemon runs
// it as them, unsandboxed, on every attach.
//
// Warnings are grouped LAST rather than printed beside their key, and the key
// listing is capped, so a padded key set cannot scroll the dangerous line out of
// view: whatever the repository does to the listing, the warnings are the final
// thing on screen above the prompt. A no-op when the project asks for nothing.
func printTrustedPolicy(root string, spec config.ProjectPolicySpec, base config.Config) {
	if spec.IsEmpty() {
		return
	}
	fmt.Printf("about to trust these %d capability-granting setting(s) in %s:\n", len(spec), root)
	lines := spec.Describe()
	for i, line := range lines {
		if i == policyDisclosureLimit {
			fmt.Printf("    … and %d more (see `plumb config show --workspace %s`)\n", len(lines)-i, root)
			break
		}
		fmt.Printf("    %s\n", line)
	}
	printPolicyWarnings(spec, base)
}

// printPolicyWarnings prints the warned keys as a block of their own. Only a key
// plumb recognises as dangerous warns, so this block is bounded by the real
// capability surface rather than by how many keys the repository chose to write.
func printPolicyWarnings(spec config.ProjectPolicySpec, base config.Config) {
	type warned struct{ key, why string }
	var ws []warned
	for _, e := range spec {
		if w := e.Warning(base); w != "" {
			ws = append(ws, warned{e.Key, w})
		}
	}
	if len(ws) == 0 {
		return
	}
	fmt.Printf("\n!! %d of these grant capability:\n", len(ws))
	for _, w := range ws {
		fmt.Printf("    %s\n        %s\n", w.key, w.why)
	}
}

// confirmTrust requires an explicit answer before the grant is recorded.
//
// Printing the disclosure and granting anyway made the "read it before answering
// for it" instruction a fiction: there was nothing to answer, and
// `plumb trust > /dev/null` granted in silence. That was arguable when the grant
// covered task commands; it is not, now that one grant covers the argv of a
// process spawned as the user on every attach and the destructive and network git
// tiers.
//
// A non-interactive stdin is REFUSED rather than auto-accepted, so a script or an
// agent pipeline cannot acquire the grant by side effect — --yes is the only way
// to say yes without a terminal, and saying it is a deliberate act.
func confirmTrust(root string) error {
	if trustAssumeYes {
		return nil
	}
	if !stdinIsTerminal() {
		return nonInteractiveTrustError(root)
	}
	fmt.Print("\ntrust these settings? [y/N] ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return trustAnswerDecision(answer)
}

// nonInteractiveTrustError is the refusal when there is no terminal to ask.
func nonInteractiveTrustError(root string) error {
	return fmt.Errorf("refusing to grant trust for %s without confirmation: stdin is not a terminal; "+
		"re-run with --yes if you have reviewed the settings above", root)
}

// trustAnswerDecision maps the typed answer to grant (nil) or refuse.
//
// Everything that is not an explicit yes refuses, including an empty line and a
// read that ended early — the prompt is [y/N], and a grant that can be obtained
// by an unreadable or absent answer is the silent grant this prompt replaced.
// The read error is deliberately not distinguished from a blank line: there is no
// answer either way, and the outcome must not differ.
//
// An empty answer names --yes, because that is what a redirected stdin produces.
// `/dev/null` is a character device, so it satisfies the terminal check and
// reaches this path rather than the explicit non-interactive refusal; the outcome
// is the same refusal, and the advice should be too.
func trustAnswerDecision(answer string) error {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	case "":
		return errors.New("trust not granted (no answer); re-run with --yes to grant without a prompt")
	default:
		return errors.New("trust not granted")
	}
}

// stdinIsTerminal reports whether stdin is an interactive terminal rather than a
// pipe or file.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
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
