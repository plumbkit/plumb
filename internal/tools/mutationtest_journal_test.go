package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
)

// mutationtest_journal_test.go covers PLAN-459's acceptance: a mutant that outlives
// its run is put back on the next daemon start, a file someone else edited is left
// alone and reported, and an entry whose file is already clean is simply forgotten.
//
// Every test points the state dir at a temp dir through XDG_STATE_HOME, so a test
// can never write into the developer's real journal — and the entries a test leaves
// can never be swept in anger.

// journalFixture points the state dir at a temp dir, writes a file, journals a
// mutant for it and leaves the mutant applied: exactly the state a killed run
// leaves behind.
func journalFixture(t *testing.T, content, mutated string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "subject.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	journalTarget(t, path, content, mutated)
	return path
}

// journalTarget journals a mutant for path and leaves it applied, using whichever
// state dir the test has already selected — so two targets can share one journal.
func journalTarget(t *testing.T, path, content, mutated string) {
	t.Helper()
	tgt := mutationTarget{path: path, original: []byte(content), mode: 0o644, sha: sha256Hex([]byte(content))}
	if err := journalMutant(tgt, mutated); err != nil {
		t.Fatalf("journaling the mutant: %v", err)
	}
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
}

// journalEntryCount is how many entries the journal holds right now.
func journalEntryCount(t *testing.T) int {
	t.Helper()
	dir, err := mutantJournalDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			n++
		}
	}
	return n
}

func readSubject(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSweepMutantJournal_RestoresAMutantThatOutlivedItsRun(t *testing.T) {
	const original, mutant = "func f() int { return 1 }\n", "func f() int { return 2 }\n"
	path := journalFixture(t, original, mutant)

	restored, needAttention, err := SweepMutantJournal()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(restored) != 1 || restored[0] != path {
		t.Errorf("restored = %v, want exactly [%s]", restored, path)
	}
	if len(needAttention) != 0 {
		t.Errorf("nothing should need attention: %v", needAttention)
	}
	if got := readSubject(t, path); got != original {
		t.Errorf("file = %q, want the pre-mutation content", got)
	}
	if n := journalEntryCount(t); n != 0 {
		t.Errorf("journal still holds %d entry(ies) after a successful restore", n)
	}
}

func TestSweepMutantJournal_LeavesAndReportsAForeignEdit(t *testing.T) {
	path := journalFixture(t, "func f() int { return 1 }\n", "func f() int { return 2 }\n")
	const theirs = "func f() int { return 3 } // edited after the run\n"
	if err := os.WriteFile(path, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	restored, needAttention, err := SweepMutantJournal()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(restored) != 0 {
		t.Errorf("a third party's edit must never be overwritten: %v", restored)
	}
	if len(needAttention) != 1 || needAttention[0] != path {
		t.Errorf("the path must be reported, not silently kept: %v", needAttention)
	}
	if got := readSubject(t, path); got != theirs {
		t.Errorf("file = %q, want the third party's content untouched", got)
	}
	if n := journalEntryCount(t); n != 1 {
		t.Errorf("the entry must survive so the report is not lost (have %d)", n)
	}
}

// TestSweepMutantJournal_ForgetsAnEntryWhoseFileIsAlreadyClean covers the other
// side of the kill window: journaled, then killed before the mutant was written.
func TestSweepMutantJournal_ForgetsAnEntryWhoseFileIsAlreadyClean(t *testing.T) {
	path := journalFixture(t, "x\n", "y\n")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	restored, needAttention, err := SweepMutantJournal()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(restored) != 0 || len(needAttention) != 0 {
		t.Errorf("nothing happened to this file, so nothing is worth reporting: %v / %v", restored, needAttention)
	}
	if n := journalEntryCount(t); n != 0 {
		t.Errorf("journal still holds %d entry(ies)", n)
	}
}

func TestSweepMutantJournal_ForgetsAnEntryWhoseFileIsGone(t *testing.T) {
	path := journalFixture(t, "x\n", "y\n")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	restored, needAttention, err := SweepMutantJournal()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(restored) != 0 || len(needAttention) != 0 {
		t.Errorf("a deleted file has no mutant to undo: %v / %v", restored, needAttention)
	}
	if n := journalEntryCount(t); n != 0 {
		t.Errorf("journal still holds %d entry(ies)", n)
	}
}

// TestMutantJournalStates_ClassifiesEveryEntry is the read-only query the reconnect
// note uses (PLAN-459). It must tell apart the four things a journalled file can be:
// still holding the mutant (say so, so the caller re-reads it), already back (there is
// nothing to undo), gone, or a third party's edit — the one case the sweep leaves alone.
func TestMutantJournalStates_ClassifiesEveryEntry(t *testing.T) {
	const original, mutant = "func f() int { return 1 }\n", "func f() int { return 2 }\n"
	const theirs = "func f() int { return 3 } // someone else's edit\n"
	cases := []struct {
		name  string
		leave func(t *testing.T, path string)
		want  MutantJournalState
	}{
		{"still applied", func(t *testing.T, _ string) { t.Helper() }, MutantStillApplied},
		{"already back", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
		}, MutantAlreadyBack},
		{"someone else's edit", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(theirs), 0o644); err != nil {
				t.Fatal(err)
			}
		}, MutantFileChanged},
		{"the file is gone", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, MutantFileGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := journalFixture(t, original, mutant)
			tc.leave(t, path)

			states, err := MutantJournalStates()
			if err != nil {
				t.Fatalf("MutantJournalStates: %v", err)
			}
			if len(states) != 1 || states[0].Path != path {
				t.Fatalf("states = %v, want exactly one entry for %s", states, path)
			}
			if states[0].State != tc.want {
				t.Errorf("state = %d, want %d (%s)", states[0].State, tc.want, tc.name)
			}
		})
	}
}

// TestMutantJournalStates_SaysWhenAFileMatchesNeitherSide covers the entry the sweep
// refuses to touch: it survives, and the note must report it in the state whose wording
// says plumb left the file exactly as it was.
func TestMutantJournalStates_SaysWhenAFileMatchesNeitherSide(t *testing.T) {
	path := journalFixture(t, "func f() int { return 1 }\n", "func f() int { return 2 }\n")
	const theirs = "func f() int { return 3 } // someone else's edit\n"
	if err := os.WriteFile(path, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	states, err := MutantJournalStates()
	if err != nil {
		t.Fatalf("MutantJournalStates: %v", err)
	}
	if len(states) != 1 || states[0].Path != path || states[0].State != MutantFileChanged {
		t.Fatalf("states = %v, want exactly [{%s %d}]", states, path, MutantFileChanged)
	}

	// The sweep leaves that entry alone (the file matches neither side), so it is
	// still reported afterwards — the note and the sweep tell the same story.
	if _, needAttention, err := SweepMutantJournal(); err != nil || len(needAttention) != 1 {
		t.Fatalf("sweep = (%v, %v), want the one path needing attention", needAttention, err)
	}
	if states, err = MutantJournalStates(); err != nil || len(states) != 1 || states[0].State != MutantFileChanged {
		t.Errorf("after the sweep the entry must survive for the note: %v / %v", states, err)
	}
}

func TestMutantJournalStates_IsEmptyWhenNothingIsJournalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	states, err := MutantJournalStates()
	if err != nil {
		t.Fatalf("MutantJournalStates: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %v, want none", states)
	}
}

// TestMutantJournalStates_SkipsEntriesItCannotRead keeps this query to what it can
// prove: an entry that will not unmarshal is the SWEEP's to report, and the note is
// never told about a file the classifier could not even name.
func TestMutantJournalStates_SkipsEntriesItCannotRead(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, err := mutantJournalDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-an-entry.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}

	states, err := MutantJournalStates()
	if err != nil {
		t.Fatalf("MutantJournalStates: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %v, want none", states)
	}
}

// TestJournalMutant_LeavesNoPartialEntry pins the atomicity of the entry write: the entry
// appears complete or not at all, and the temp file it was staged in never survives as
// something a reader could mistake for a journal entry.
func TestJournalMutant_LeavesNoPartialEntry(t *testing.T) {
	path := journalFixture(t, "x\n", "y\n")
	dir, err := mutantJournalDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var jsonFiles, tempFiles int
	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case ".json":
			jsonFiles++
		case ".tmp":
			tempFiles++
		}
	}
	if jsonFiles != 1 {
		t.Errorf("want exactly one journal entry, found %d", jsonFiles)
	}
	if tempFiles != 0 {
		t.Errorf("the temp file used for the atomic write survived (%d) — a reader must never see it", tempFiles)
	}
	// The entry is readable and names the target: a truncated or half-renamed file would fail here.
	states, err := MutantJournalStates()
	if err != nil {
		t.Fatalf("MutantJournalStates: %v", err)
	}
	if len(states) != 1 || states[0].Path != path {
		t.Errorf("states = %+v, want exactly the one entry for %s", states, path)
	}
}

// TestSweepMutantJournal_ReportsAnEntryItCannotParse covers the state the sweep used to
// swallow: an entry that cannot be read or parsed means a mutant was applied and plumb
// cannot say where — so it must be REPORTED (by its own file name) and KEPT, because
// deleting it would destroy the only record that something was mutated.
func TestSweepMutantJournal_ReportsAnEntryItCannotParse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, err := mutantJournalDir()
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	restored, needAttention, err := SweepMutantJournal()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(restored) != 0 {
		t.Errorf("nothing can be restored from an unparsable entry: %v", restored)
	}
	if len(needAttention) != 1 || needAttention[0] != bad {
		t.Errorf("needAttention = %v, want the entry file %s", needAttention, bad)
	}
	if _, statErr := os.Stat(bad); statErr != nil {
		t.Errorf("the unparsable entry must be kept so the record survives: %v", statErr)
	}
}

// TestClearMutantJournal_ForgetsOnlyItsOwnTarget keeps two mutants in flight apart:
// one file per entry means one target's completion cannot erase another's record.
func TestClearMutantJournal_ForgetsOnlyItsOwnTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	first := filepath.Join(dir, "a.go")
	journalTarget(t, first, "a\n", "A\n")
	second := filepath.Join(dir, "b.go")
	journalTarget(t, second, "b\n", "B\n")
	if n := journalEntryCount(t); n != 2 {
		t.Fatalf("two mutants in flight need two entries, have %d", n)
	}

	clearMutantJournal(first)
	if n := journalEntryCount(t); n != 1 {
		t.Errorf("one entry must remain (have %d)", n)
	}
	restored, needAttention, err := SweepMutantJournal()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(restored) != 1 || restored[0] != second {
		t.Errorf("restored = %v, want exactly [%s]", restored, second)
	}
	if len(needAttention) != 0 {
		t.Errorf("nothing needs attention: %v", needAttention)
	}
	if got := readSubject(t, second); got != "b\n" {
		t.Errorf("the other target was not restored: %q", got)
	}
	// The cleared target keeps its mutant: clearing is what the tool does AFTER a
	// verified restore, and this test simulates the two happening independently.
	if got := readSubject(t, first); got != "A\n" {
		t.Errorf("first = %q, want its mutant left alone", got)
	}
}

// --- the wiring: a real run, driven through the kill window -------------------
//
// Everything above builds the journal state BY HAND and so exercises the SWEEP.
// What none of it can see is whether runOne still journals a mutant before
// writing it and still clears the entry after a verified restore: delete either
// call and every test above stays green, because none of them runs runOne at
// all. These three do — one driven into the window, one as its control, and one
// refusing a run whose journal cannot be written.

// killInWindow makes the next run stop inside the mutate window: the mutant is
// journalled and on disk, and the deferred restore is skipped, which is exactly
// what a killed process leaves behind (mutationKillWindowHook).
func killInWindow(t *testing.T) {
	t.Helper()
	mutationKillWindowHook = func() bool { return true }
	t.Cleanup(func() { mutationKillWindowHook = nil })
}

// TestMutantJournal_AKilledRunLeavesAMutantTheSweepCanRestore is PLAN-459's
// acceptance for the WIRING rather than for the sweep: a REAL run — preflight,
// baseline, journal, mutate, compile, test — is killed in the window, and what
// it leaves on disk is put back by the next daemon start's sweep.
func TestMutantJournal_AKilledRunLeavesAMutantTheSweepCanRestore(t *testing.T) {
	const original, mutant = "keep\n", "changed\n"
	env := newMutationEnv(t, original)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	killInWindow(t)

	report, err := env.run(t, "keep", "changed")
	if err != nil {
		t.Fatalf("a run killed in the window is not a failure of the call itself: %v\n%s", err, report)
	}
	// The two things a kill leaves behind, asserted before the sweep touches
	// anything: the mutant on disk, and its entry in the journal.
	if got := env.content(t); got != mutant {
		t.Fatalf("the file must still hold the mutant a killed run left: %q, want %q", got, mutant)
	}
	states, err := MutantJournalStates()
	if err != nil {
		t.Fatalf("MutantJournalStates: %v", err)
	}
	if len(states) != 1 || states[0].Path != env.file || states[0].State != MutantStillApplied {
		t.Fatalf("the journal must hold the killed run's entry, still applied: %v, want exactly [{%s %d}]",
			states, env.file, MutantStillApplied)
	}

	// The next daemon start: the file goes back to its pre-run content and the
	// entry is spent.
	restored, needAttention, err := SweepMutantJournal()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(restored) != 1 || restored[0] != env.file {
		t.Errorf("restored = %v, want exactly [%s]", restored, env.file)
	}
	if len(needAttention) != 0 {
		t.Errorf("nothing needs a person here: %v", needAttention)
	}
	if got := env.content(t); got != original {
		t.Errorf("file = %q, want the pre-run content", got)
	}
	if n := journalEntryCount(t); n != 0 {
		t.Errorf("the sweep resolved the entry, so it must be gone (have %d)", n)
	}
}

// TestMutantJournal_AnOrdinaryRunLeavesNoEntry is the control the kill test
// needs to mean anything: a run that is NOT killed restores its file and clears
// its entry, so the journal is empty afterwards. Without this, an implementation
// that journals and never clears would leave the sweep restoring a file that was
// already correct — and the kill test above would still pass.
func TestMutantJournal_AnOrdinaryRunLeavesNoEntry(t *testing.T) {
	const original = "keep\n"
	env := newMutationEnv(t, original)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	report, err := env.run(t, "keep", "changed")
	if err != nil {
		t.Fatalf("an ordinary run: %v\n%s", err, report)
	}
	if got := env.content(t); got != original {
		t.Fatalf("an unkilled run restores its file: got %q, want %q", got, original)
	}
	if n := journalEntryCount(t); n != 0 {
		t.Errorf("a run whose restore was verified must clear its entry (have %d)", n)
	}
	if states, err := MutantJournalStates(); err != nil || len(states) != 0 {
		t.Errorf("the reconnect note must have nothing to report: %v / %v", states, err)
	}
}

// TestMutationTest_RefusesWhenTheJournalCannotBeWritten covers the other half of
// the wiring: with an unusable state dir every mutant used to come back invalid
// — the mutant never ran — so a whole-run environment fault was reported once
// per mutant as though each mutant had a problem of its own. The call is refused
// instead, before anything runs.
//
// The state dir is made unusable with a REGULAR FILE where the directory should
// be: creating a directory below it fails with ENOTDIR for every user, root
// included, where a read-only directory would be no obstacle at all to root.
func TestMutationTest_RefusesWhenTheJournalCannotBeWritten(t *testing.T) {
	env := newMutationEnv(t, "keep\n")
	// Proof that nothing ran, rather than only a claim in the message.
	env.installScript(t, env.compileScript, "touch \"$(dirname \"$0\")/compile-ran\"\nexit 0")
	blocker := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(blocker, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blocker)

	report, err := env.run(t, "keep", "changed")
	if err == nil {
		t.Fatalf("a run that cannot journal must be refused, not reported: %q", report)
	}
	if stateDir := paths.StateDir(); !strings.Contains(err.Error(), stateDir) {
		t.Errorf("the refusal must name the state directory %s:\n%v", stateDir, err)
	}
	if !strings.Contains(err.Error(), "NOTHING WAS RUN") {
		t.Errorf("the refusal must say that nothing was run, so it is not read as a per-mutant verdict:\n%v", err)
	}
	if _, err := os.Stat(filepath.Join(env.root, "compile-ran")); !os.IsNotExist(err) {
		t.Errorf("the refusal must come before any command runs (compile-ran: %v)", err)
	}
	if got := env.content(t); got != "keep\n" {
		t.Errorf("refused calls mutate nothing: got %q", got)
	}
}
