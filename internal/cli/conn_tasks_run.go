package cli

// conn_tasks_run.go — the {run} and {verbose} placeholders in a stored
// [tasks.<lang>] command (#538): a test-name filter and a verbose flag, the two
// things {target} could not express, since it fills ONE positional (in practice a
// package). Before these, running one test meant abandoning run_task for a shell.
//
// Both follow {target}'s rule that an absent value adds nothing, so a shipped
// default carrying them builds the argv it always did. Unlike {target} they carry
// a FLAG rather than a default, because what they stand for, when absent, is the
// absence of that flag:
//
//	{run}           the filter as one element; omitted when none is given
//	{run:<flag>}    `<flag> <filter>` (go test -run X, pytest -k X, cargo test -- X)
//	{verbose:<f>}   `<f>` when verbose is asked for, else nothing
//
// A run filter given to a command with no {run} is refused, as a target is: a
// silently unfiltered run reports a green over tests nobody asked about. verbose
// is only noted, since ignoring it changes how much is printed, never what ran.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
)

const (
	runToken           = "{run}"
	runTokenPrefix     = "{run:"
	verboseTokenPrefix = "{verbose:"
)

// taskScope is what a caller asks a stored command to be narrowed or widened by.
type taskScope struct {
	target  string
	run     string
	verbose bool
}

// runPlaceholder recognises a whole-element {run} or {run:<flag>}, returning the
// flag ("" for the bare form).
func runPlaceholder(arg string) (flag string, ok bool) {
	if arg == runToken {
		return "", true
	}
	if strings.HasPrefix(arg, runTokenPrefix) && strings.HasSuffix(arg, "}") {
		return arg[len(runTokenPrefix) : len(arg)-1], true
	}
	return "", false
}

// verbosePlaceholder recognises a whole-element {verbose:<flag>} with a non-empty
// flag. A bare {verbose} would say nothing about what to add, so it is not one.
func verbosePlaceholder(arg string) (flag string, ok bool) {
	if strings.HasPrefix(arg, verboseTokenPrefix) && strings.HasSuffix(arg, "}") && len(arg) > len(verboseTokenPrefix)+1 {
		return arg[len(verboseTokenPrefix) : len(arg)-1], true
	}
	return "", false
}

// isScopePlaceholder reports whether arg is a {run} or {verbose} placeholder:
// the elements that vanish from an unscoped argv.
func isScopePlaceholder(arg string) bool {
	_, isRun := runPlaceholder(arg)
	_, isVerbose := verbosePlaceholder(arg)
	return isRun || isVerbose
}

// stripScopePlaceholders returns argv without its {run} and {verbose}
// placeholders — the argv an unscoped run builds from them.
func stripScopePlaceholders(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if !isScopePlaceholder(a) {
			out = append(out, a)
		}
	}
	return out
}

// errNoRunPlaceholder is the sentinel for "a run filter was given but the command
// has no {run}", replaced at the resolver by runPlaceholderRefusal.
var errNoRunPlaceholder = errors.New("a run filter was given but the command has no {run} placeholder")

// substituteRunVerbose fills the {run} and {verbose} placeholders, dropping each
// that has no value. It must run on EVERY task argv, scoped or not, or a literal
// `{run:-run}` would reach the command.
func substituteRunVerbose(argv []string, run string, verbose bool) ([]string, error) {
	out := make([]string, 0, len(argv)+1)
	found := false
	for _, a := range argv {
		if flag, ok := runPlaceholder(a); ok {
			found = true
			if run != "" && flag != "" {
				out = append(out, flag)
			}
			if run != "" {
				out = append(out, run)
			}
			continue
		}
		if flag, ok := verbosePlaceholder(a); ok {
			if verbose {
				out = append(out, flag)
			}
			continue
		}
		out = append(out, a)
	}
	if run != "" && !found {
		return nil, errNoRunPlaceholder
	}
	return out, nil
}

// runPlaceholderRefusal explains a refused run filter the way
// targetPlaceholderRefusal explains a refused target: the stored command, the
// file it came from, and the placeholder to add.
func runPlaceholderRefusal(ws string, tc config.TasksConfig, lang, slot string) error {
	stored := strings.TrimSpace(tc.Get(slot))
	return fmt.Errorf(
		"run_task %s: a run filter was given but the stored %s command for %s has no {run} placeholder. "+
			"Stored command: %q (from %s). Add one where the runner takes a test-name filter — "+
			"{run:-run} for go test, {run:-k} for pytest, {run:--} for cargo test — under [tasks.%s] %s, "+
			"or call without run",
		slot, slot, lang, stored, taskCommandSource(ws, lang, slot, stored, config.DefaultTaskCommand(lang, slot)), lang, slot)
}

// verboseIgnoredNote states that verbose was asked for and the command has no
// {verbose:<flag>} to take it, so the output is the command's usual.
func verboseIgnoredNote(tc config.TasksConfig, lang, slot string) (string, bool) {
	argv, err := taskArgvTemplate(tc, lang, slot)
	if err != nil || argv == nil {
		return "", false
	}
	for _, a := range argv {
		if _, ok := verbosePlaceholder(a); ok {
			return "", false
		}
	}
	return fmt.Sprintf("verbose was NOT applied: the stored %s command for %s has no {verbose:<flag>} placeholder "+
		"(e.g. {verbose:-v}); add one under [tasks.%s] %s", slot, lang, lang, slot), true
}
