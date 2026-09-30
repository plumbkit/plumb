package cli

// conn_tasks_notes.go — the notes run_task attaches to a response: what the
// resolver did with a call that the caller cannot see in the argv it gets back.
// Split from conn_tasks.go, which keeps resolution and the trust gate.

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
)

// taskNotes reports what run_task did with this call that the caller cannot see
// from the argv it gets back: a target or run filter that was accepted but not
// applied, a stored command plumb rewrote to make one land, and a verbose flag
// the command had nowhere to put.
//
// All are silent rewrites of the caller's intent, and a silent rewrite is the
// failure family this file exists to shrink (see testTargetStyle). None is worth
// a REFUSAL — a refusal opens a new rejection cluster, which is the exact thing
// being shrunk — so each is stated in the response instead. With nothing asked
// there is nothing to say: reconciliation is argv-identical unscoped, and a
// composite had nothing to drop.
func taskNotes(tc config.TasksConfig, lang, slot string, sc taskScope) []string {
	var notes []string
	if n, ok := compositeScopeNote(tc, lang, slot, sc); ok {
		notes = append(notes, n)
	}
	if n, ok := reconciledPlaceholderNote(tc, lang, slot, sc); ok {
		notes = append(notes, n)
	}
	if _, composite := compositeSubSlots(slot); sc.verbose && !composite {
		if n, ok := verboseIgnoredNote(tc, lang, slot); ok {
			notes = append(notes, n)
		}
	}
	return notes
}

// compositeScopeNote states that a composite slot ran its sub-commands without
// the caller's target or run filter, and names the sub-slot that WOULD have taken
// them.
//
// run_task(slot:"verify", target:…) accepted the target, discarded it, ran the
// whole suite and reported success — a green over a scope the caller never
// asked for, which this file elsewhere calls worse than the hardcoded command it
// replaced. The sub-slots are asked the same question run_task would ask, so the
// recommendation cannot claim a scope the workspace does not actually offer.
func compositeScopeNote(tc config.TasksConfig, lang, slot string, sc taskScope) (string, bool) {
	subs, ok := compositeSubSlots(slot)
	if !ok || (sc.target == "" && sc.run == "") {
		return "", false
	}
	var scopable []string
	for _, sub := range subs {
		if steps, err := buildScopedTaskSteps(tc, lang, sub, taskScope{target: sc.target, run: sc.run}); err == nil && len(steps) > 0 {
			scopable = append(scopable, sub)
		}
	}
	remedy := fmt.Sprintf("no sub-slot of %s takes that scope in this workspace, so there is nothing to scope it to", slot)
	if len(scopable) > 0 {
		remedy = fmt.Sprintf("call run_task again with slot %q and the same scope",
			strings.Join(scopable, `" or "`))
	}
	return fmt.Sprintf(
		"%s was NOT applied: %s is a composite that runs %s in sequence and has no single "+
			"command for a scope to land in, so every step below ran unscoped, over everything. To scope, %s.",
		describeScope(sc), slot, strings.Join(subs, " then "), remedy), true
}

// describeScope names the scope a call asked for, for a note that says it was
// dropped.
func describeScope(sc taskScope) string {
	switch {
	case sc.run == "":
		return fmt.Sprintf("the target %q", sc.target)
	case sc.target == "":
		return fmt.Sprintf("the run filter %q", sc.run)
	default:
		return fmt.Sprintf("the target %q and the run filter %q", sc.target, sc.run)
	}
}

// reconciledPlaceholderNote states that plumb ran this call through the shipped
// default's placeholders because the stored command is that default with them
// written out or left out (reconcileTargetPlaceholder), and the call used one the
// stored command lacks.
//
// Reconciliation rewrites a command the user wrote. That it is provably
// meaning-preserving is why it is allowed; it is not a reason to do it silently,
// and the schema has no byte budget left to say so, so the response says it.
func reconciledPlaceholderNote(tc config.TasksConfig, lang, slot string, sc taskScope) (string, bool) {
	stored, err := config.ParseTaskCommand(tc.Get(slot))
	if err != nil || stored == nil {
		return "", false
	}
	reconciled := reconcileTargetPlaceholder(stored, lang, slot)
	if reconciled == nil {
		return "", false
	}
	_, _, storedHasTarget := soleDefaultedPlaceholder(stored)
	if (sc.target == "" || storedHasTarget) && sc.run == "" && !sc.verbose {
		return "", false
	}
	return fmt.Sprintf(
		"the stored %s command for %s (%q) is plumb's own default %q with its placeholders written out "+
			"or left out, so plumb ran this call through that default's placeholders instead of refusing it. "+
			"An unscoped run builds the identical argv either way; write the placeholders into "+
			"[tasks.%s] %s to make it explicit.",
		slot, lang, strings.Join(stored, " "), strings.Join(reconciled, " "), lang, slot), true
}
