package tools

import (
	"os"
	"path/filepath"
	"testing"
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

// TestMutantJournalPaths_ListsWhatIsStillOutThere is the read-only query the
// reconnect note uses (PLAN-459): a resolved entry must vanish from it, and an
// entry the sweep refused to touch must stay.
func TestMutantJournalPaths_ListsWhatIsStillOutThere(t *testing.T) {
	path := journalFixture(t, "func f() int { return 1 }\n", "func f() int { return 2 }\n")
	const theirs = "func f() int { return 3 } // someone else's edit\n"
	if err := os.WriteFile(path, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, err := MutantJournalPaths()
	if err != nil {
		t.Fatalf("MutantJournalPaths: %v", err)
	}
	if len(paths) != 1 || paths[0] != path {
		t.Fatalf("paths = %v, want exactly [%s]", paths, path)
	}

	// The sweep leaves that entry alone (the file matches neither side), so it is
	// still reported afterwards — the note and the sweep tell the same story.
	if _, needAttention, err := SweepMutantJournal(); err != nil || len(needAttention) != 1 {
		t.Fatalf("sweep = (%v, %v), want the one path needing attention", needAttention, err)
	}
	if paths, err = MutantJournalPaths(); err != nil || len(paths) != 1 {
		t.Errorf("after the sweep the entry must survive for the note: %v / %v", paths, err)
	}
}

func TestMutantJournalPaths_IsEmptyWhenNothingIsJournalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	paths, err := MutantJournalPaths()
	if err != nil {
		t.Fatalf("MutantJournalPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none", paths)
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
