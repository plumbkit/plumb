package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// mutationtest_journal.go makes mutation_test's restoration survive a killed
// process (PLAN-459).
//
// The tool's contract is that a mutant is restored on every exit path, and a
// deferred restore does cover every path a running process can take. It cannot
// cover the process being killed — a daemon restart mid-run left a mutant in
// internal/tools/transaction.go, and the client's "re-read the file" advice named
// neither the file nor the mutant.
//
// So the mutant is JOURNALLED BEFORE it is applied: the path, the sha256 the file
// had, the sha256 the mutant will have, the original bytes and the mode. The entry
// is removed once the file is verifiably restored. Whatever is left when the daemon
// starts again is a mutant that outlived its run, and SweepMutantJournal puts those
// files back — but only when the file still holds the mutant's own sha256: a file
// someone else has edited since (matching neither side) is left alone and reported,
// because guessing which content the user wants would be worse than saying so.
//
// One file per entry, named by the target's sha256, under the daemon's state dir:
// two mutants in flight cannot corrupt each other's entry, and no lock is needed to
// keep them apart. The transaction log for transaction_apply is the precedent for
// the shape; this journal is deliberately simpler, because a mutant is one file
// with three known states rather than a transaction of many.

// mutantJournalDirName is the journal's directory under the state dir.
const mutantJournalDirName = "mutant-journal"

// mutantJournalEntry is one applied mutant, as recorded before it was applied.
type mutantJournalEntry struct {
	Path      string    `json:"path"`
	ShaBefore string    `json:"sha_before"`
	ShaMutant string    `json:"sha_mutant"`
	Original  []byte    `json:"original"`
	Mode      uint32    `json:"mode"`
	At        time.Time `json:"at"`
}

// mutantJournalDir returns the journal directory, creating it if needed.
func mutantJournalDir() (string, error) {
	dir := filepath.Join(paths.StateDir(), mutantJournalDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mutant journal: %w", err)
	}
	return dir, nil
}

// journalPathFor names one target's entry. Hashing the path keeps a target with a
// slash, a space or a newline in it from naming a file anywhere but here.
func journalPathFor(dir, target string) string {
	sum := sha256.Sum256([]byte(target))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

// mutantDigest is the journal's own digest spelling: the tool compares digests as
// hex strings, and this is the one place they are computed. It is named for the
// journal rather than for the algorithm because the test files already carry a
// sha256Hex helper, and two same-named helpers in one package is how the
// duplicate-primitive gate earns its keep.
func mutantDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// journalMutant records the pre-mutation state. It is called BEFORE the mutant is
// written, so a process killed between the two leaves a journal entry with nothing
// mutated — which the sweep resolves by finding the original sha already in place.
func journalMutant(tgt mutationTarget, mutated string) error {
	dir, err := mutantJournalDir()
	if err != nil {
		return err
	}
	entry := mutantJournalEntry{
		Path:      tgt.path,
		ShaBefore: tgt.sha,
		ShaMutant: mutantDigest([]byte(mutated)),
		Original:  tgt.original,
		Mode:      uint32(tgt.mode.Perm()),
		At:        time.Now().UTC(),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("mutant journal: %w", err)
	}
	if err := os.WriteFile(journalPathFor(dir, tgt.path), data, 0o600); err != nil {
		return fmt.Errorf("mutant journal: %w", err)
	}
	return nil
}

// clearMutantJournal forgets a target's entry: the file is back to the state the
// entry describes, or no mutant was ever applied to it.
func clearMutantJournal(target string) {
	if dir, err := mutantJournalDir(); err == nil {
		_ = os.Remove(journalPathFor(dir, target))
	}
}

// checkMutantJournalUsable refuses a whole run whose journal cannot be written,
// BEFORE any mutant is applied or any command is run.
//
// The journal is not optional bookkeeping: journalMutant runs before the mutant
// is written, and its failure is reported per mutant as `invalid`. So an
// unusable state directory — a sandbox that denies $HOME, a state dir on a
// read-only mount, an XDG_STATE_HOME pointing at a file — turns EVERY mutant
// into an invalid, with a reason carrying the journal's own I/O error. The
// caller reads that as "my mutants are wrong" and is sent to correct them, when
// the truth is one environment fault that has nothing to do with any mutant,
// reported once per mutant as though each had a problem of its own. It is the
// tool's own false-attribution defect, arrived at from the filesystem.
//
// It probes by CREATING and REMOVING a file rather than by asking whether the
// directory exists, because an existing but unwritable state dir satisfies
// MkdirAll and still fails at the first entry. The probe's name carries no
// .json extension, so a probe left behind by an unlink that failed is never
// read as an entry by the sweep or by MutantJournalPaths.
func checkMutantJournalUsable() error {
	stateDir := paths.StateDir()
	dir, err := mutantJournalDir()
	if err != nil {
		return mutantJournalUnusable(stateDir, err)
	}
	probe, err := os.CreateTemp(dir, ".probe-")
	if err != nil {
		return mutantJournalUnusable(stateDir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// mutantJournalUnusable builds that refusal. It names the directory that cannot
// hold the journal and says plainly that nothing was run, so a caller is never
// left reading a whole-run fault as a per-mutant verdict.
func mutantJournalUnusable(stateDir string, cause error) error {
	return fmt.Errorf("mutation_test: NOTHING WAS RUN — the mutant journal at %s (under the state directory %s) "+
		"cannot be written, so this call is refused before any mutant is applied and before any command is run. "+
		"The journal is what puts a killed run's mutant back at the next daemon start; without it every mutant "+
		"would come back invalid for a reason that has nothing to do with the mutant. Make that directory "+
		"writable — or point XDG_STATE_HOME at one that is — and retry. Cause: %w",
		filepath.Join(stateDir, mutantJournalDirName), stateDir, cause)
}

// MutantJournalPaths names the files the journal still holds — the entries a sweep
// could not resolve, because the file matches neither the pre-mutation nor the
// mutant content (someone edited it since) or because restoring it failed. It is
// read-only: the reconnect note needs to say what is still out there without
// sweeping anything itself.
func MutantJournalPaths() ([]string, error) {
	dir, err := mutantJournalDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("mutant journal: %w", err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			continue
		}
		var entry mutantJournalEntry
		if json.Unmarshal(data, &entry) != nil || entry.Path == "" {
			continue
		}
		paths = append(paths, entry.Path)
	}
	return paths, nil
}

// sweepAction is what the sweep decided about one entry.
type sweepAction int

const (
	sweepNothing   sweepAction = iota // the file is already as it should be
	sweepRestored                     // the mutant was undone, verified by digest
	sweepAttention                    // left alone, and worth telling the user about
)

// SweepMutantJournal restores every journalled file that still holds its mutant's
// content, and returns the paths it restored and the paths that need a person.
//
// It runs when the daemon starts, which is the only moment that can know a run was
// killed: everything in the journal belongs to a process that no longer exists.
func SweepMutantJournal() (restored, needAttention []string, err error) {
	dir, err := mutantJournalDir()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("mutant journal: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		full := filepath.Join(dir, e.Name())
		data, readErr := os.ReadFile(full)
		if readErr != nil {
			continue
		}
		var entry mutantJournalEntry
		if json.Unmarshal(data, &entry) != nil {
			continue // an unreadable entry is not a reason to touch a file
		}
		switch sweepOne(entry) {
		case sweepNothing:
			_ = os.Remove(full)
		case sweepRestored:
			_ = os.Remove(full)
			restored = append(restored, entry.Path)
		case sweepAttention:
			needAttention = append(needAttention, entry.Path)
		}
	}
	return restored, needAttention, nil
}

// sweepOne decides one entry's fate. The three states are exhaustive: the file
// holds the original digest (nothing to do), the mutant digest (put the original
// back and prove it landed), or something else (a third party's edit — leave it).
func sweepOne(entry mutantJournalEntry) sweepAction {
	sha, err := fileSHA256(entry.Path)
	if err != nil {
		// Gone, or unreadable: there is no mutant left to undo.
		return sweepNothing
	}
	switch sha {
	case entry.ShaBefore:
		return sweepNothing
	case entry.ShaMutant:
	default:
		return sweepAttention
	}
	if err := os.WriteFile(entry.Path, entry.Original, os.FileMode(entry.Mode)); err != nil {
		return sweepAttention
	}
	got, err := fileSHA256(entry.Path)
	if err != nil || got != entry.ShaBefore {
		return sweepAttention
	}
	return sweepRestored
}
