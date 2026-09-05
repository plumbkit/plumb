package cli

// conn_tasks.go wires the run_task tool to the session: it resolves a slot to a
// runnable command for the workspace's primary language and applies the
// per-workspace trust gate to project-supplied commands. Mirrors the gitPolicy
// closure pattern (config adapted into a plain tools type at the cli seam).

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// taskResolver resolves slot (+ optional target) to a runnable command for this
// session's workspace. Default- and global-supplied commands always run; a
// command the project's .plumb/config.toml overrides must be trusted first
// (plumb trust).
//
// language names which [tasks.<lang>] block to read, or "" for the workspace's
// primary. The primary is the right default and was long the only option, but
// it is not always the language the caller means: a polyglot repo resolves ONE
// primary, so every sibling language's commands — the shipped defaults
// included — were unreachable through run_task, and an agent that wanted the
// Python tests in a TypeScript-primary repo had to abandon the tool and shell
// out, losing the no-shell argv contract and the trust gate with it.
func (s *connSession) taskResolver(slot, target, language string) (tools.TaskCommand, error) {
	ws := s.workspace()
	if ws == "" {
		return tools.TaskCommand{}, errors.New("run_task: no workspace is pinned for this session")
	}
	v := s.view()
	lang, err := taskLanguage(v, language)
	if err != nil {
		return tools.TaskCommand{}, err
	}
	tc := v.tasks[lang]
	steps, err := taskStepsOrRefusal(ws, tc, lang, slot, target)
	if err != nil {
		return tools.TaskCommand{}, err
	}
	if len(steps) == 0 {
		// No command for this slot. Hand back the context the tool needs to say
		// WHICH language it resolved for and what that language does have, rather
		// than a bare "not configured for this workspace".
		return tools.TaskCommand{
			Slot: slot, Language: lang, Configured: configuredSlots(tc, lang),
			ConfigPath: config.ProjectConfigPath(ws),
		}, nil
	}
	workdir, wdErr := commandWorkdir(ws, tc.WorkingDir)
	if wdErr != nil {
		return tools.TaskCommand{}, fmt.Errorf("run_task %s: %w", slot, wdErr)
	}
	provenance, fromProject := taskProvenance(ws, lang, slot)
	if fromProject {
		cmds, cmdErr := config.ProjectTaskCommands(ws)
		if cmdErr != nil {
			return tools.TaskCommand{}, fmt.Errorf("run_task: reading project task commands: %w", cmdErr)
		}
		if !config.NewTrustStore().IsTrustedForTasks(ws, cmds) {
			return tools.TaskCommand{}, fmt.Errorf(
				"run_task: the %s command for %s comes from this project's .plumb/config.toml and is not trusted "+
					"(or the project's task commands changed since `plumb trust` was last run). "+
					"review them, then run `plumb trust` in %s to allow this project's task commands", slot, lang, ws)
		}
	}
	return tools.TaskCommand{
		Slot: slot, Steps: steps, Provenance: provenance, WorkingDir: workdir,
		Language: lang, Configured: configuredSlots(tc, lang),
		ConfigPath: config.ProjectConfigPath(ws),
		Notes:      taskNotes(tc, lang, slot, target),
	}, nil
}

// taskLanguage picks the [tasks.<lang>] block a run_task call resolves against:
// the requested language when one was given, else the workspace's primary.
//
// A requested language is checked against the languages this workspace actually
// has commands for, NOT against the language set or the detected primary. Those
// are different questions, and the one the caller is asking is "can you run my
// Python tests" — which is answerable whenever [tasks.python] has a command,
// whether or not pyright is installed, whether or not Python is the primary,
// and whether or not detection saw Python at all. Gating on the LSP set instead
// would refuse the shipped default commands on exactly the polyglot repos this
// argument exists for.
func taskLanguage(v sessionView, requested string) (string, error) {
	if requested == "" {
		lang := v.acquiredLanguage
		if lang == "" || lang == LanguageNone {
			return "", fmt.Errorf("run_task: no language is attached to this workspace, so there is no primary [tasks.<lang>] block to resolve against. "+
				"Name one explicitly with the language argument (%s), or attach a language with session_start",
				languagesWithCommands(v))
		}
		return lang, nil
	}
	if tc, ok := v.tasks[requested]; ok && len(configuredSlots(tc, requested)) > 0 {
		return requested, nil
	}
	return "", fmt.Errorf("run_task: no [tasks.%s] commands are configured for this workspace (%s). "+
		"Set one with [tasks.%s] <slot> = \"...\" in %s, then run `plumb trust` in the workspace "+
		"if you wrote it to the project's config",
		requested, languagesWithCommands(v), requested, config.ProjectConfigPath(v.acquiredRoot))
}

// languagesWithCommands renders the languages this workspace can actually run a
// task for, as the remedy half of a refusal. Sorted so the message is stable.
func languagesWithCommands(v sessionView) string {
	var langs []string
	for lang, tc := range v.tasks {
		if len(configuredSlots(tc, lang)) > 0 {
			langs = append(langs, lang)
		}
	}
	if len(langs) == 0 {
		return "no language has task commands configured here"
	}
	sort.Strings(langs)
	return "languages with commands: " + strings.Join(langs, ", ")
}

// taskStepsOrRefusal builds a slot's steps, replacing the bare
// "no {target} placeholder" sentinel with the refusal that names the stored
// command and the file holding it.
//
// It exists so there is ONE such seam rather than two. The resolver and the CLI
// each used to do this mapping themselves, and the mapping is invisible to every
// test that calls either the pure message builder or the pure step builder — so
// deleting it from a caller left that caller silently emitting the bare sentence
// this card exists to retire, with the whole suite still green.
func taskStepsOrRefusal(ws string, tc config.TasksConfig, lang, slot, target string) ([][]string, error) {
	steps, err := buildTaskSteps(tc, lang, slot, target)
	if errors.Is(err, errNoTargetPlaceholder) {
		return nil, targetPlaceholderRefusal(ws, tc, lang, slot)
	}
	return steps, err
}

// taskNotes reports what run_task did with this call that the caller cannot see
// from the argv it gets back: a target that was accepted but not applied, and a
// stored command plumb rewrote to make the target land.
//
// Both are silent rewrites of the caller's intent, and a silent rewrite is the
// failure family this file exists to shrink (see testTargetStyle). Neither is
// worth a REFUSAL — a refusal opens a new rejection cluster, which is the exact
// thing being shrunk — so each is stated in the response instead. With no target
// there is nothing to say: reconciliation is argv-identical unscoped, and a
// composite had nothing to drop.
func taskNotes(tc config.TasksConfig, lang, slot, target string) []string {
	if target == "" {
		return nil
	}
	var notes []string
	if n, ok := compositeTargetNote(tc, lang, slot, target); ok {
		notes = append(notes, n)
	}
	if n, ok := reconciledPlaceholderNote(tc, lang, slot); ok {
		notes = append(notes, n)
	}
	return notes
}

// compositeTargetNote states that a composite slot ran its sub-commands
// unscoped, and names the sub-slot that WOULD have taken the target.
//
// run_task(slot:"verify", target:…) accepted the target, discarded it, ran the
// whole suite and reported success — a green over a scope the caller never
// asked for, which this file elsewhere calls worse than the hardcoded command it
// replaced. The sub-slots are asked the same question run_task would ask, so the
// recommendation cannot claim a scope the workspace does not actually offer.
func compositeTargetNote(tc config.TasksConfig, lang, slot, target string) (string, bool) {
	subs, ok := compositeSubSlots(slot)
	if !ok {
		return "", false
	}
	var scopable []string
	for _, sub := range subs {
		if steps, err := buildTaskSteps(tc, lang, sub, target); err == nil && len(steps) > 0 {
			scopable = append(scopable, sub)
		}
	}
	remedy := fmt.Sprintf("no sub-slot of %s takes a target in this workspace, so there is nothing to scope it to", slot)
	if len(scopable) > 0 {
		remedy = fmt.Sprintf("call run_task again with slot %q and the same target",
			strings.Join(scopable, `" or "`))
	}
	return fmt.Sprintf(
		"the target %q was NOT applied: %s is a composite that runs %s in sequence and has no single "+
			"command for a target to land in, so every step below ran unscoped, over everything. To scope, %s.",
		target, slot, strings.Join(subs, " then "), remedy), true
}

// reconciledPlaceholderNote states that plumb restored its own {target:<D>}
// placeholder in the stored command to make this scoped run possible.
//
// Reconciliation rewrites a command the user wrote. That it is provably
// meaning-preserving (see reconcileTargetPlaceholder) is why it is allowed; it
// is not a reason to do it silently, and the schema has no byte budget left to
// say so, so the response says it.
func reconciledPlaceholderNote(tc config.TasksConfig, lang, slot string) (string, bool) {
	stored, err := config.ParseTaskCommand(tc.Get(slot))
	if err != nil || stored == nil {
		return "", false
	}
	reconciled := reconcileTargetPlaceholder(stored, lang, slot)
	if reconciled == nil {
		return "", false
	}
	idx, _, ok := soleDefaultedPlaceholder(reconciled)
	if !ok {
		return "", false
	}
	return fmt.Sprintf(
		"the stored %s command for %s (%q) is plumb's own default with the %s placeholder written out as "+
			"its default value, so plumb scoped this run through that placeholder instead of refusing the "+
			"target. An unscoped run builds the identical argv either way; write the placeholder into "+
			"[tasks.%s] %s to make it explicit.",
		slot, lang, strings.Join(stored, " "), reconciled[idx], lang, slot), true
}

// taskState reports the resolved run_task / run_command surface for this
// session, for the session_start orientation section. It answers from the same
// view the resolvers read, so the report and the behaviour cannot disagree.
//
// Unreachable is the point of it: task resolution keys on the single primary
// language, so in a monorepo the other detected languages' commands — including
// the shipped defaults — cannot be run through run_task at all. That was
// invisible, while the identity line happily listed every language.
func (s *connSession) taskState() tools.TaskState {
	v := s.view()
	lang := v.acquiredLanguage
	if lang == "none" {
		lang = ""
	}
	st := tools.TaskState{Language: lang}
	if lang != "" {
		st.Configured = configuredSlots(v.tasks[lang], lang)
	}
	for _, other := range v.discoveredLangs {
		if other != lang && other != "" && other != "none" {
			st.Unreachable = append(st.Unreachable, other)
		}
	}
	for _, c := range v.commands {
		st.Commands = append(st.Commands, c.Name)
	}
	return st
}

// targetAcceptanceProbe is a syntactically valid target used only to ask whether
// the test command has a slot for one. It is never run.
const targetAcceptanceProbe = "./..."

// testScope reports how this session's workspace runs a SCOPED test command, so
// topology_affected can emit a target run_task will accept instead of assuming
// `go test`. A session with no language attached yields the zero value, which
// the tool renders as bare directories and no command.
//
// The language rule lives HERE, not in internal/tools, because this is the only
// layer that can see both the language and the configured command.
func (s *connSession) testScope() tools.TestScope {
	v := s.view()
	lang := v.acquiredLanguage
	if lang == "" || lang == LanguageNone {
		return tools.TestScope{}
	}
	tc := v.tasks[lang]
	return tools.TestScope{
		Language:   lang,
		WorkingDir: tc.WorkingDir,
		Style:      testTargetStyle(lang, tc),
	}
}

// testTargetStyle decides how a package directory should be spelled for this
// workspace's test command, or TargetNone when it cannot be spelled safely.
//
// Two conditions, and BOTH are needed. The command must take a positional path
// operand (testSlotTakesPositionalTarget), and the language's runner must treat
// that operand as a PATH. Checking only the first is what let
// `[tasks.go] test = "go test -run {target}"` through: the probe proves a
// {target} slot exists, never that it means a directory, so the emitted package
// path landed in a test-NAME regex, matched nothing, and exited 0 — a silent
// green over zero tests, which is worse than the hardcoded `go test` this
// replaced. Checking only the second mis-handles a project that rewired its own
// command.
//
// rust is excluded even though `cargo test <filter>` is positional, because the
// filter matches test NAMES. typescript, swift and zig ship no placeholder and
// scope through project-specific flags.
func testTargetStyle(lang string, tc config.TasksConfig) tools.TargetStyle {
	if !testSlotTakesPositionalTarget(tc, lang) {
		return tools.TargetNone
	}
	switch lang {
	case "go":
		return tools.TargetGoPackage
	case "python":
		return tools.TargetPath
	default:
		return tools.TargetNone
	}
}

// testSlotTakesPositionalTarget reports whether the test command has a {target}
// placeholder that is a positional OPERAND rather than the value of a flag.
//
// Acceptance is asked of buildTaskSteps — the same function run_task uses —
// rather than re-derived, because configuredSlots below records what happens
// otherwise: two hand-written predicates disagreed with buildTaskSteps in
// opposite directions. The len(steps) check matters for the same reason it does
// there — an unset or whitespace-only command parses to a nil argv and returns
// (nil, nil), so err == nil alone reports "takes a target" for a workspace that
// has no test command at all.
//
// Position is then checked directly, since no existing function answers it.
func testSlotTakesPositionalTarget(tc config.TasksConfig, lang string) bool {
	steps, err := buildTaskSteps(tc, lang, "test", targetAcceptanceProbe)
	if err != nil || len(steps) == 0 {
		return false
	}
	// The RECONCILED template, not the raw parse. Asking the raw command would
	// split this predicate from run_task's own builder for exactly the configs
	// reconciliation exists for: `[tasks.go] test = "go test ./..."` builds a
	// scoped argv above and would report "cannot be narrowed to a directory"
	// here, so topology_affected would keep emitting bare directories to a
	// run_task that now accepts targets. Same argv, one question.
	argv, err := taskArgvTemplate(tc, lang, "test")
	if err != nil {
		return false
	}
	return placeholderIsPositionalOperand(argv)
}

// placeholderIsPositionalOperand reports whether argv's {target} element is a
// trailing positional operand.
//
// A placeholder preceded by a flag that takes a separate value (`-run {target}`,
// `-k {target}`) is that flag's VALUE, and a directory handed to it selects
// nothing while still exiting 0. A flag carrying its own value (`-count=1`) does
// not consume what follows, so `go test -count=1 {target:./...}` is positional —
// treating every leading dash as consuming would refuse a perfectly good
// command.
//
// Deliberately conservative beyond that: a placeholder that is not last
// (`go test {target} -v`) is treated as unknown rather than guessed at, because
// naming the directory costs a caller one edit while a wrong target costs them a
// green run that tested nothing.
func placeholderIsPositionalOperand(argv []string) bool {
	for i, a := range argv {
		if _, ok := targetPlaceholder(a); !ok {
			continue
		}
		if i == 0 || consumesNextArg(argv[i-1]) {
			return false
		}
		return i == len(argv)-1
	}
	return false
}

// booleanTestFlags are flags of the go and python test runners that take NO
// value, so a target following one is still a positional operand.
//
// Without this, the most ordinary customisation of a shipped default silently
// killed the feature: `go test -race {target:./...}` and `pytest -q {target:.}`
// were read as "the target is -race's value", so no target was emitted and the
// caller was told the command could not be narrowed to a directory — while
// run_task would in fact have built a perfectly correct scoped argv.
//
// An unknown flag still counts as consuming. That direction is deliberate: a
// missing target costs the caller one manual edit, while a target handed to a
// flag that wanted a value runs the wrong tests and reports success. Adding a
// flag here is safe; guessing about one is not.
var booleanTestFlags = map[string]bool{
	// go test
	"-v": true, "-race": true, "-short": true, "-cover": true, "-benchmem": true,
	"-json": true, "-failfast": true, "-shuffle": false, "-count": false,
	// pytest
	"-q": true, "-x": true, "-s": true, "-l": true, "--verbose": true,
	"--quiet": true, "--exitfirst": true, "--no-header": true, "--tb": false,
}

// consumesNextArg reports whether an argv element is a flag whose value is the
// FOLLOWING element.
//
// Three things do not consume: a flag spelled with "=" (it carries its own
// value), "--" (the canonical marker that what follows is positional — reading
// it as consuming was precisely backwards), and a known boolean flag.
func consumesNextArg(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	if arg == "--" || strings.Contains(arg, "=") {
		return false
	}
	if takesNoValue, known := booleanTestFlags[arg]; known {
		return !takesNoValue
	}
	return true
}

// configuredSlots lists the task slots that actually have a command, in a fixed
// order: the built-ins first, then the project's own extra slots. Both are
// asked the same question, so an extra is reported — and refused — on exactly
// the same terms as a built-in.
//
// It answers by asking buildTaskSteps — the same function run_task uses — rather
// than by re-deriving the condition. Two hand-written predicates disagreed with
// it in opposite directions: `tc.Build != "" && tc.Test != ""` was stricter than
// the verify branch, which happily runs whichever half is set, so a
// build-only config reported "verify: not configured" for a call that in fact
// runs; and `tc.Get(slot) != ""` was looser than ParseTaskCommand, which trims
// first, so a whitespace-only command reported as configured for a call that is
// refused. A session_start section that contradicts the tool it describes is
// worse than no section, so there is now one source of truth.
func configuredSlots(tc config.TasksConfig, lang string) []string {
	var out []string
	for _, slot := range config.ConfiguredSlotNames(tc) {
		steps, err := buildTaskSteps(tc, lang, slot, "")
		if err != nil || len(steps) == 0 {
			continue
		}
		out = append(out, slot)
	}
	return out
}

// compositeSlots names the sub-slots a composite slot runs, in order. A
// composite stores no command of its own (config.TasksConfig.Get returns "" for
// it), so every place that has to answer "what does this slot actually run?" —
// the step builder, the trust gate's provenance lookup, the CLI listing, and the
// note that says a target was not applied — reads it from here rather than
// re-spelling `slot == "verify"` and drifting.
var compositeSlots = map[string][]string{"verify": {"build", "test"}}

// compositeSubSlots reports the sub-slots slot runs, and whether it is composite
// at all.
func compositeSubSlots(slot string) ([]string, bool) {
	subs, ok := compositeSlots[slot]
	return subs, ok
}

// buildTaskSteps turns a slot into the argv steps to run. verify is the
// composite build-then-test; every other slot is a single command.
func buildTaskSteps(tc config.TasksConfig, lang, slot, target string) ([][]string, error) {
	if subs, ok := compositeSubSlots(slot); ok {
		var steps [][]string
		for _, sub := range subs {
			// The target is deliberately not threaded into a sub-step: a composite
			// has no single command for one to land in, and picking a sub-step to
			// scope would report a partial run as a whole one. It is not silently
			// dropped either — compositeTargetNote says so in the response.
			argv, err := taskStep(tc, lang, sub, "")
			if err != nil {
				return nil, err
			}
			if argv != nil {
				steps = append(steps, argv)
			}
		}
		return steps, nil
	}
	argv, err := taskStep(tc, lang, slot, target)
	if err != nil {
		return nil, err
	}
	if argv == nil {
		return nil, nil
	}
	return [][]string{argv}, nil
}

// taskStep parses one slot's command into an argv and applies the {target}
// substitution. A nil argv means the slot is unset.
func taskStep(tc config.TasksConfig, lang, slot, target string) ([]string, error) {
	argv, err := taskArgvTemplate(tc, lang, slot)
	if err != nil {
		return nil, err
	}
	if argv == nil {
		return nil, nil
	}
	return substituteTarget(argv, target)
}

// taskProvenance reports the layer a slot's command comes from and whether the
// project overrides it (so the trust gate applies). verify consults build+test.
// It asks ProjectTaskCommands — the same enumeration the trust hash binds to —
// rather than looking the key up by name, and compares case-INSENSITIVELY.
//
// The old version called config.ProjectValuePresent(ws, {"tasks", lang, slot}),
// an exact map lookup, and that was a gate bypass. go-toml/v2 binds a table name
// to a struct field case-insensitively, so a cloned repository shipping
// `[TASKS.go] test = "..."` had its command decoded into Config.Tasks and run by
// run_task, while the exact lookup missed the raw key: fromProject came back
// false and the trust check below was skipped entirely. The command was also
// absent from ProjectTaskCommands, so it was never in the hash either — both
// halves of the defence missed the same spelling. Arbitrary code execution from
// an untrusted clone, and the sibling of the [[COMMAND]] bypass in
// conn_commands.go.
//
// A read error fails CLOSED (treated as project-supplied, so the trust gate
// applies). A gate that cannot determine provenance must not assume the safe
// answer.
func taskProvenance(ws, lang, slot string) (label string, fromProject bool) {
	slots := []string{slot}
	if subs, ok := compositeSubSlots(slot); ok {
		slots = subs
	}
	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		return "project", true
	}
	for _, c := range cmds {
		if !strings.EqualFold(c.Lang, lang) {
			continue
		}
		// A project working_dir makes EVERY slot of that language project-supplied,
		// including the ones whose command comes from the shipped defaults.
		//
		// Without this the gate has a hole the size of the feature: working_dir is
		// not a command, so a project that overrides no command at all still
		// decides WHERE the default `go build ./...` runs. Provenance answers "did
		// this project influence what is about to execute?", and choosing the
		// directory is influence — the argv is only half of what runs.
		if strings.EqualFold(c.Slot, taskWorkingDirKey) {
			return "project", true
		}
		for _, sl := range slots {
			if strings.EqualFold(c.Slot, sl) {
				return "project", true
			}
		}
	}
	return "config", false
}

// taskWorkingDirKey is the [tasks.<lang>] key that is not a command slot.
// ProjectTaskCommands sweeps every string under the table, so it arrives as a
// TaskCommandSpec with this Slot — which is what puts it in the trust hash, and
// what taskProvenance matches on above.
const taskWorkingDirKey = "working_dir"
