package cli

// conn_tasks_target.go — everything about the {target} placeholder in a stored
// [tasks.<lang>] command: parsing it, substituting a caller's target into it,
// restoring it in a stored command that spells out plumb's own default, and the
// refusal (with its remedy) when a target was given and no slot exists for one.
//
// Split from conn_tasks.go, which keeps slot resolution, the trust gate and the
// session-facing reporting. The two halves are joined only by taskStep and
// taskStepsOrRefusal.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
)

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
	if fixed, ok := targetPlaceholderRemedy(stored, shipped); ok {
		remedy = fmt.Sprintf("keep your own command and put plumb's placeholder into it — "+
			"[tasks.%s] %s = %q — or call run_task without a target", lang, slot, fixed)
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

// targetPlaceholderRemedy returns the CALLER'S OWN stored command with plumb's
// defaulted {target:<D>} placeholder put into it — substituted for the operand D
// where the caller wrote one, appended where the shipped default's D is empty
// (there being no element to replace). It reports false when nothing can be
// derived: no shipped default, no defaulted placeholder in it, an unparseable
// stored command, or a stored command that never mentions D.
//
// It exists because naming the shipped default as the value to SET destroys
// whatever the caller already had, and — because reconciliation now handles the
// stored command that IS the expanded default — the only commands that can still
// reach this refusal are commands that DIFFER from it. So the old advice
// ("restore the placeholder plumb ships for it, <default>") was wrong in every
// case that could actually reach it: following it turned `go test -race ./...`
// into `go test {target:./...}` and silently dropped the race detector from
// every later run, `go test ./... -tags=integration` into a different suite
// entirely, and `gotestsum ./...` into a different runner. Deriving the remedy
// from the caller's own argv keeps their flags by construction, and for a
// caller who had no extra flags it lands on exactly the shipped default anyway.
func targetPlaceholderRemedy(stored, shipped string) (string, bool) {
	if !shippedTakesTarget(shipped) {
		return "", false
	}
	def, err := config.ParseTaskCommand(shipped)
	if err != nil || def == nil {
		return "", false
	}
	idx, operand, ok := soleDefaultedPlaceholder(def)
	if !ok {
		return "", false
	}
	argv, err := config.ParseTaskCommand(stored)
	if err != nil || argv == nil {
		return "", false
	}
	token := def[idx]
	if operand == "" {
		// An empty default means the shipped command spells "everything" as the
		// ABSENCE of the operand, so there is nothing to substitute for: the
		// placeholder is appended, and with no target it collapses away again,
		// leaving the caller's command exactly as it is today.
		return strings.Join(append(slices.Clone(argv), token), " "), true
	}
	at := slices.Index(argv, operand)
	if at < 0 {
		// The caller's command does not spell the default's operand at all
		// (`make test`), so there is no element that can be identified as the
		// scope. Guessing one would put the target somewhere it does not belong;
		// the generic remedy is left to say "add a placeholder" instead.
		return "", false
	}
	out := slices.Clone(argv)
	out[at] = token
	return strings.Join(out, " "), true
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
