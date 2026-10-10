package cli

import (
	"log/slog"

	"github.com/plumbkit/plumb/internal/tools"
)

// daemon_mutant_sweep.go runs mutation_test's crash recovery (PLAN-459).
//
// mutation_test restores its mutant on every exit path a running process can take.
// It cannot restore one when the process is killed — a daemon restart mid-run left
// a mutant sitting in a source file, and the client's advice ("re-read the file to
// check whether it landed") named neither the file nor the mutant. The tool
// therefore journals a mutant before applying it (mutationtest_journal.go), and
// this sweep is the other half: whatever entry survives into a new daemon belongs
// to a process that no longer exists, so the file it names is either holding a
// mutant that must go back or holding something a person changed since.
//
// It runs once, at daemon start, beside logRecoveredHijacks — the same kind of
// startup reckoning, and the only moment that can tell "killed" from "in flight".

// sweepKilledMutants restores the mutants a killed run left behind and logs both
// outcomes, because the second one needs a person: the file no longer matches
// either the pre-mutation or the mutant content, so plumb reports it and leaves it
// exactly as it found it.
func sweepKilledMutants() {
	restored, needAttention, err := tools.SweepMutantJournal()
	if err != nil {
		slog.Warn("daemon: mutant journal sweep failed; a mutant may still be on disk", "err", err)
		return
	}
	for _, path := range restored {
		slog.Warn("daemon: restored a mutant a killed mutation_test left behind", "path", path)
	}
	for _, att := range needAttention {
		// Two fields, never one: "path" is the file whose content is in question and is empty when
		// the entry could not be parsed, while "journal_entry" is always the file holding the
		// pre-mutation bytes. Reporting one key for both meanings is the defect the typed result
		// fixed (PLAN-459).
		slog.Error("daemon: a killed mutation_test left a mutant and the file has changed since — leaving it alone; "+
			"the pre-mutation content is kept in the journal entry named here",
			"path", att.Path, "journal_entry", att.Entry)
	}
}
