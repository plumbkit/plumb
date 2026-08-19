package cli

// trust_cmd.go — `plumb trust`: the per-workspace grant that lets a project's
// .plumb/config.toml supply task commands, named commands, shell policy,
// [xcode]/[lsp.<lang>] process argv, git tiers, and collab switches. The
// disclosure below is the informed-consent record that gate rests on: the exact
// content the grant binds to, values and all, printed before the Yes/No
// selector — restyled to the shared CLI presentation (tui styles, ┊ rows, the
// stop/restart confirmation pattern) with the disclosure semantics unchanged.

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// trustAssumeYes skips the confirmation prompt (`plumb trust --yes`). It is the
// only way to grant without a terminal — see confirmTrust.
var trustAssumeYes bool

var trustCmd = &cobra.Command{
	Use:   "trust [directory]",
	Short: "Trust everything this workspace's .plumb/config.toml supplies",
	Long: `Approve the settings a project's .plumb/config.toml supplies that plumb
otherwise ignores. This is ONE grant per workspace, and it covers all of them:

  · [tasks.<lang>]        build/lint/test/e2e commands run by run_task
  · [[command]]           the named command allow-list run by run_command
  · [commands]            the shell policy — allow_shell and deny_network
  · [xcode]               auto_build_server, which runs xcodebuild here
  · [lsp.<lang>]          command, args, env, initialization_options and the
                          root markers — the argv of a process plumb spawns
  · [git]                 the destructive and network tiers, and the
                          protected-branch list
  · [collab]              the cross-agent channel switches

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
	tui.RebuildStyles()
	printTrustedTaskCommands(root, cmds)
	printTrustedPolicy(root, spec, cfg)
	if len(cmds) == 0 && spec.IsEmpty() {
		fmt.Println(tui.MutedStyle.Render(root + " supplies nothing that needs trust; recording the grant anyway so a later addition re-prompts"))
	} else if err := confirmTrust(root); err != nil {
		return err
	}
	if err := config.NewTrustStore().SetTrustedForProject(root, cmds, spec); err != nil {
		return err
	}
	// Trust is bound to the exact content above: changing any of it invalidates
	// that binding and re-prompts. A trust.json upgraded from the old boolean
	// format re-confirms here once.
	printTrustGrantSummary(root)
	return nil
}

// printTrustGrantSummary is the post-grant record: what the grant covers, and
// that it is bound to the content shown above — on the shared section style
// (● heading, ┊ rows) so the disclosure and the record read as one surface.
func printTrustGrantSummary(root string) {
	fmt.Println()
	fmt.Println(tui.HintStyle.Render("● Trusted " + root))
	fmt.Println()
	for _, line := range []string{
		"This grant covers the project's task commands,",
		"its [[command]] allow-list,",
		"its [commands] shell policy,",
		"its [xcode] build-server settings,",
		"its [lsp.<lang>] server command/args/env,",
		"its [git] tier policy and its [collab] channel switches.",
		"",
		"Every one of them is bound to the content shown above;",
		"changing any of it requires re-running `plumb trust`,",
		"which will show you what changed before you approve it again.",
	} {
		fmt.Printf("  %s %s\n", tui.SepStyle.Render("┊"), tui.MutedStyle.Render(line))
	}
}

// policyDisclosureLimit caps the per-key listing. The key set is attacker-chosen
// — a repository can pad [git] with any number of junk keys, all captured by the
// deliberate whole-table extraction — so an uncapped listing is a way to push the
// lines that matter off the user's scrollback.
const policyDisclosureLimit = 40

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

	tui.RebuildStyles()
	fmt.Println(tui.HintStyle.Render("● Task commands in " + render.ContractPath(root)))
	fmt.Println()
	for _, lang := range langs {
		fmt.Printf("  %s\n", tui.ItemStyle.Render("["+lang+"]"))
		printLangTaskCommands(byLang[lang])
	}
	fmt.Println()
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
		fmt.Printf("  %s %s  %s\n",
			tui.SepStyle.Render("┊"),
			tui.ItemStyle.Render(render.PadRight(slot, 8)),
			tui.MutedStyle.Render(display))
		if argv, perr := config.ParseTaskCommand(cmd); perr == nil && config.FlagsInlineInterpreter(argv) {
			fmt.Printf("  %s %s\n", tui.SepStyle.Render("┊"),
				tui.WarnStyle.Render("!! WARNING: this runs an interpreter with inline code ("+argv[0]+") — arbitrary code execution by design; trust only if you wrote it"))
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
	tui.RebuildStyles()
	fmt.Println(tui.HintStyle.Render("● Capability settings in " + render.ContractPath(root)))
	fmt.Println()
	lines := spec.Describe()
	for i, line := range lines {
		if i == policyDisclosureLimit {
			fmt.Printf("  %s %s\n", tui.SepStyle.Render("┊"),
				tui.MutedStyle.Render(fmt.Sprintf("… and %d more (see `plumb config show --workspace %s`)", len(lines)-i, root)))
			break
		}
		fmt.Printf("  %s %s\n", tui.SepStyle.Render("┊"), tui.MutedStyle.Render(line))
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
	fmt.Printf("\n%s\n\n", tui.WarnStyle.Render(fmt.Sprintf("!! %d of these grant capability:", len(ws))))
	for _, w := range ws {
		fmt.Printf("  %s %s\n", tui.SepStyle.Render("┊"), tui.WarnStyle.Render(w.key))
		fmt.Printf("        %s\n", tui.MutedStyle.Render(w.why))
	}
}

// confirmTrust requires an explicit Yes before the grant is recorded.
//
// Printing the disclosure and granting anyway made the "read it before answering
// for it" instruction a fiction: there was nothing to answer, and
// `plumb trust > /dev/null` granted in silence. That was arguable when the grant
// covered task commands; it is not, now that one grant covers the argv of a
// process spawned as the user on every attach and the destructive and network
// git tiers.
//
// A non-interactive stdin is REFUSED rather than auto-accepted, so a script or an
// agent pipeline cannot acquire the grant by side effect — --yes is the only way
// to say yes without a terminal, and saying it is a deliberate act. The selector
// defaults to No, and only an explicit Yes (the y key, or moving to Yes and
// pressing enter) grants.
func confirmTrust(root string) error {
	if trustAssumeYes {
		return nil
	}
	if !stdinIsTerminal() {
		return nonInteractiveTrustError(root)
	}
	confirmed, err := runTrustConfirmationSelector()
	if err != nil {
		return fmt.Errorf("confirming trust: %w", err)
	}
	if !confirmed {
		return errors.New("trust not granted")
	}
	return nil
}

// nonInteractiveTrustError is the refusal when there is no terminal to ask.
func nonInteractiveTrustError(root string) error {
	return fmt.Errorf("refusing to grant trust for %s without confirmation: stdin is not a terminal; "+
		"re-run with --yes if you have reviewed the settings above", root)
}

// stdinIsTerminal reports whether stdin is an interactive terminal rather than
// a pipe, file, or /dev/null — the last is a character device, so the
// ModeCharDevice heuristic it replaced counted it as a terminal.
func stdinIsTerminal() bool {
	return term.IsTerminal(os.Stdin.Fd())
}

// runTrustConfirmationSelector runs the shared Yes/No gate (confirm.go) with
// the trust prompt. See confirmTrust for why it defaults to No.
func runTrustConfirmationSelector() (bool, error) {
	return runYesNoSelector(renderTrustConfirmation)
}

func renderTrustConfirmation(cursor int) string {
	tui.RebuildStyles()
	warnBadge := lipgloss.NewStyle().
		Foreground(tui.ActiveTheme.SelectionBackground).
		Background(tui.ActiveTheme.Warning).
		Bold(true).
		Render(" ! ")
	options := []string{"Yes", "No"}
	optionLines := make([]string, 0, len(options))
	for i, option := range options {
		marker := "  "
		style := tui.HintStyle
		if i == cursor {
			marker = "❯ "
			style = tui.SelectedStyle
		}
		optionLines = append(optionLines, tui.SepStyle.Render("    ┊ ")+style.Render(marker+option))
	}
	return fmt.Sprintf("\n%s %s\n%s\n%s\n%s\n",
		warnBadge,
		tui.ItemStyle.Render("Trust these settings?"),
		tui.MutedStyle.Render("    granting binds plumb to the exact content above; changing any of it re-prompts"),
		optionLines[0],
		optionLines[1],
	)
}
