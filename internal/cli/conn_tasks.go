package cli

// conn_tasks.go wires the run_task tool to the session: it resolves a slot to a
// runnable command for the workspace's primary language and applies the
// per-workspace trust gate to project-supplied commands. Mirrors the gitPolicy
// closure pattern (config adapted into a plain tools type at the cli seam).

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// taskResolver resolves slot (+ optional target) to a runnable command for this
// session's workspace and primary language. Default- and global-supplied
// commands always run; a command the project's .plumb/config.toml overrides
// must be trusted first (plumb trust).
func (s *connSession) taskResolver(slot, target string) (tools.TaskCommand, error) {
	ws := s.workspace()
	lang := s.view().acquiredLanguage
	if ws == "" || lang == "" || lang == "none" {
		return tools.TaskCommand{}, errors.New("run_task: no language detected for this workspace; configure [tasks.<lang>] and attach a language")
	}
	tc := s.view().tasks[lang]
	steps, err := buildTaskSteps(tc, lang, slot, target)
	if err != nil {
		if errors.Is(err, errNoTargetPlaceholder) {
			return tools.TaskCommand{}, targetPlaceholderRefusal(ws, tc, lang, slot)
		}
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
	workdir, err := commandWorkdir(ws, tc.WorkingDir)
	if err != nil {
		return tools.TaskCommand{}, fmt.Errorf("run_task %s: %w", slot, err)
	}
	provenance, fromProject := taskProvenance(ws, lang, slot)
	if fromProject {
		cmds, err := config.ProjectTaskCommands(ws)
		if err != nil {
			return tools.TaskCommand{}, fmt.Errorf("run_task: reading project task commands: %w", err)
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
	}, nil
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

// buildTaskSteps turns a slot into the argv steps to run. verify is the
// composite build-then-test; every other slot is a single command.
func buildTaskSteps(tc config.TasksConfig, lang, slot, target string) ([][]string, error) {
	if slot == "verify" {
		var steps [][]string
		for _, sub := range []string{"build", "test"} {
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

// taskArgvTemplate parses a slot's stored command into an argv and, when that
// command is the shipped default with its {target:<D>} placeholder written out
// as D, restores the placeholder. A nil argv means the slot is unset.
func taskArgvTemplate(tc config.TasksConfig, lang, slot string) ([]string, error) {
	argv, err := config.ParseTaskCommand(tc.Get(slot))
	if err != nil || argv == nil {
		return nil, err
	}
	if reconciled := reconcileTargetPlaceholder(argv, lang, slot); reconciled != nil {
		return reconciled, nil
	}
	return argv, nil
}

// reconcileTargetPlaceholder restores the shipped default's {target:<D>}
// placeholder in a stored command that spells that element out as its own
// default value. It returns nil when the stored command is anything else.
//
// This is the fix for the largest non-policy run_task failure family. A user
// (or a template, or a copied snippet) writes `[tasks.go] test = "go test ./..."`
// — the command plumb itself shipped before the placeholder existed — and every
// scoped call is then refused, permanently, because that argv has no slot for a
// target. The advertised topology_affected -> run_task(target:) handoff is
// therefore broken for that workspace and stays broken: nothing in the refusal
// used to say which command was stored or where it lived.
//
// The rule is deliberately an EQUIVALENCE, not a heuristic, because the two
// obvious heuristics are both wrong in the same expensive direction:
//
//   - Appending the target to any placeholder-less command turns
//     `go test ./...` into `go test ./... ./internal/cli`, which runs the WHOLE
//     suite while reporting a scoped run. A green over the wrong test set is the
//     failure mode this file already refuses to risk (see testTargetStyle).
//   - Substituting the LAST element of any command guesses at commands whose
//     final operand is not a scope at all.
//
// Requiring the stored command to be the shipped default with the placeholder
// expanded makes the outcome provable instead: with no target both forms build a
// byte-identical argv (TestReconcile_NoTargetArgvIsUnchanged pins that), and with
// a target the stored command now behaves exactly as the shipped default would
// have. Nothing is guessed and nothing new can run — the substitution lands in
// the position plumb itself designated.
//
// A command that is NOT that equivalence keeps its refusal, which is the other
// direction and matters just as much: `golangci-lint run` with a target is
// meaningless, and `go test -count=1 ./...` is a command plumb never wrote and
// must not rewrite.
func reconcileTargetPlaceholder(argv []string, lang, slot string) []string {
	def, err := config.ParseTaskCommand(config.DefaultTaskCommand(lang, slot))
	if err != nil || def == nil {
		return nil
	}
	idx, value, ok := soleDefaultedPlaceholder(def)
	if !ok {
		return nil
	}
	// An empty default means the shipped command spells "everything" as the
	// ABSENCE of the operand (cargo test, pytest), so the expanded form is one
	// element SHORTER; every other default expands in place.
	expanded := slices.Concat(def[:idx:idx], []string{value}, def[idx+1:])
	if value == "" {
		expanded = slices.Concat(def[:idx:idx], def[idx+1:])
	}
	if !slices.Equal(argv, expanded) {
		return nil
	}
	return slices.Clone(def)
}

// soleDefaultedPlaceholder returns the index and declared default of argv's
// single {target:<D>} element. It reports false for an argv with no placeholder,
// with more than one, or whose placeholder is the BARE {target} — a bare
// placeholder has no expanded spelling to compare a stored command against, so
// there is nothing to reconcile.
func soleDefaultedPlaceholder(argv []string) (idx int, value string, ok bool) {
	idx = -1
	for i, a := range argv {
		d, isPlaceholder := targetPlaceholder(a)
		if !isPlaceholder {
			continue
		}
		if idx >= 0 || d == nil {
			return 0, "", false
		}
		idx, value = i, *d
	}
	return idx, value, idx >= 0
}

// errNoTargetPlaceholder is the sentinel for "a target was given but this
// command has no slot for one". It is a sentinel rather than a bare string so
// the resolver — the only layer that can see the workspace, and therefore the
// config FILE the command came from — can replace it with a refusal that names
// the stored command and where to edit it. run_command shares substituteTarget
// and keeps the plain wording, since it has no shipped default to reconcile
// against.
var errNoTargetPlaceholder = errors.New("a target was given but the command has no {target} placeholder")

// targetPlaceholderRefusal explains a refused target in terms the caller can act
// on: the stored command, the file it came from, and what to change.
//
// The bare sentence it replaces named none of those, so a caller hitting it had
// no way to tell a command that cannot take a target from one that was simply
// written without the placeholder — and the same call failed the same way
// forever. Every occurrence of this failure in 90 days of telemetry was the
// latter.
func targetPlaceholderRefusal(ws string, tc config.TasksConfig, lang, slot string) error {
	stored := strings.TrimSpace(tc.Get(slot))
	shipped := config.DefaultTaskCommand(lang, slot)
	remedy := fmt.Sprintf("add a {target} placeholder to it under [tasks.%s] %s, or call run_task without a target", lang, slot)
	if shippedTakesTarget(shipped) {
		remedy = fmt.Sprintf("restore the placeholder plumb ships for it (%q) under [tasks.%s] %s", shipped, lang, slot)
	}
	return fmt.Errorf(
		"run_task %s: a target was given but the stored %s command for %s has no {target} placeholder. "+
			"Stored command: %q (from %s). To scope this slot, %s",
		slot, slot, lang, stored, taskCommandSource(ws, lang, slot, stored, shipped), remedy)
}

// shippedTakesTarget reports whether a shipped default command carries a
// {target} placeholder in either spelling.
func shippedTakesTarget(shipped string) bool {
	return strings.Contains(shipped, config.TargetToken) ||
		strings.Contains(shipped, config.TargetTokenPrefix)
}

// taskCommandSource names the file a slot's command came from, so the refusal
// above points at something the caller can open. It reads provenance the same
// way the trust gate does rather than re-deriving it.
func taskCommandSource(ws, lang, slot, stored, shipped string) string {
	if _, fromProject := taskProvenance(ws, lang, slot); fromProject {
		return config.ProjectConfigPath(ws)
	}
	if stored != strings.TrimSpace(shipped) {
		return config.GlobalConfigPath()
	}
	return "plumb's shipped defaults"
}

// substituteTarget replaces a {target} argv element with target. A target with
// no placeholder is an error; a placeholder with no target is an error UNLESS
// the placeholder declares a default, written {target:<default>}.
//
// The inline default is what makes scoping reachable out of the box. Every
// shipped test command was `go test ./...` — no placeholder — so run_task's
// target and mutation_test's test_target were not merely undiscoverable but an
// outright error on an unmodified install, and a mutation run therefore paid for
// the WHOLE suite per mutant.
//
// Why a default attached to the placeholder rather than one held elsewhere: the
// two alternatives both misfire. A per-language fallback applied to a bare
// {target} would silently change the meaning of commands users already wrote —
// `go test -run {target}` would quietly run everything instead of refusing — and
// a separate "scoped" slot doubles the config surface for one argument. Written
// inline, the default sits where the placeholder is and cannot drift from it,
// and a bare {target} keeps its strict contract exactly as before, so no
// existing configuration changes behaviour.
//
// An EMPTY default ({target:}) drops the element entirely. That is not a
// curiosity: for cargo test, swift test and friends, "everything" is the ABSENCE
// of a positional argument, so there is no string that could stand in for it.
func substituteTarget(argv []string, target string) ([]string, error) {
	out := make([]string, 0, len(argv))
	found := false
	for _, a := range argv {
		def, ok := targetPlaceholder(a)
		if !ok {
			out = append(out, a)
			continue
		}
		found = true
		value := target
		if value == "" {
			if def == nil {
				return nil, errors.New("this command needs a target ({target} placeholder)")
			}
			value = *def
		}
		if value == "" {
			continue // an empty default means "omit this argument"
		}
		out = append(out, value)
	}
	if target != "" && !found {
		return nil, errNoTargetPlaceholder
	}
	return out, nil
}

// targetPlaceholder recognises `{target}` and `{target:<default>}` as WHOLE argv
// elements, returning the declared default (nil when there is none).
//
// Whole-element only, matching what the plain token always accepted: the argv is
// split on whitespace with no quoting, so a placeholder glued to other text
// would substitute into an argument whose boundaries nobody can see.
func targetPlaceholder(arg string) (def *string, ok bool) {
	if arg == config.TargetToken {
		return nil, true
	}
	if !config.IsTargetToken(arg) {
		return nil, false
	}
	value := arg[len(config.TargetTokenPrefix) : len(arg)-1]
	return &value, true
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
	if slot == "verify" {
		slots = []string{"build", "test"}
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
