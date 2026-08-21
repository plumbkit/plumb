package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillsSync_MarkerRetainingUserEditInManifestlessDirIsNotOverwritten is
// the regression for a real data-loss defect found in independent review of
// PLAN-365's first version: lastShippedHash's pre-manifest fallback computed
// its "known prior hash" from the CURRENT on-disk content itself, so
// diskHash == oldHash was true BY CONSTRUCTION for any marker-stamped file —
// including one the user had hand-edited while leaving the (invisible,
// HTML-comment) marker line alone, which is the realistic case: nothing
// prompts a user to strip a line their markdown viewer never renders. A
// manifest-less directory can never prove a differing marker-stamped file is
// plumb's own (there is no historical shipped content to compare against for
// any version but the running one, and the running version's content is, by
// definition, the "new" side of the comparison) — so it must always be
// treated as a conflict: the file is left untouched, and any backup whose
// content cannot be matched to an actual recorded shipped hash must survive
// too.
func TestSkillsSync_MarkerRetainingUserEditInManifestlessDirIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	skill := embeddedSkills()[0]
	skillDir := filepath.Join(dir, skill.Name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(skillDir, "SKILL.md")

	// A user's hand edit that RETAINS the provenance marker — no manifest
	// exists yet (this directory was never synced under this mechanism, or
	// was installed by a pre-manifest plumb).
	const userEdit = "<!-- plumb: 0.10.0 -->\nuser edited this body, kept the marker line\n"
	if err := os.WriteFile(dst, []byte(userEdit), 0o600); err != nil {
		t.Fatal(err)
	}

	// A backup sitting beside it holding the same edit — the user's only
	// other copy. Deleting it on top of overwriting the live file would
	// destroy the edit twice over in one run.
	bakName := skill.Name + ".20260101-000000.bak"
	bakDir := filepath.Join(dir, bakName)
	if err := os.MkdirAll(bakDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bakDir, "SKILL.md"), []byte(userEdit), 0o600); err != nil {
		t.Fatal(err)
	}

	target := skillsTestTarget("", dir)
	_, results, cleanup := installSkillsFor(target, false)

	var found bool
	for _, r := range results {
		if r.name != skill.Name {
			continue
		}
		found = true
		if r.err != nil {
			t.Fatalf("unexpected error: %v", r.err)
		}
		if !strings.HasPrefix(r.action, skillActionConflict) {
			t.Fatalf("action = %q, want a conflict — a marker-retaining edit with no manifest entry must never be classified as plumb's own", r.action)
		}
	}
	if !found {
		t.Fatalf("no result for %s", skill.Name)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userEdit {
		t.Fatalf("SKILL.md was overwritten: got %q, want the untouched user edit %q", got, userEdit)
	}

	if _, err := os.Stat(bakDir); err != nil {
		t.Errorf(".bak with unverifiable content was deleted: %v", err)
	}
	for _, name := range cleanup.removed {
		if name == bakName {
			t.Errorf("cleanup removed an unverifiable backup: %s", name)
		}
	}
}

// TestSkillsSync_CleansShippedHashBackupsPreservesUserEditWritesPlumbNew is
// PLAN-365's literal acceptance fixture: a skills directory carrying
// directory-level ".bak" backups whose content is provably plumb's own
// (shipped-hash litter, matching what a live `~/.claude/skills` looked like)
// alongside one skill the user hand-edited. A sync must remove the matching
// backups, leave the user's edit completely untouched, write plumb's
// proposed content to a ".plumb-new" file for review instead of overwriting
// it, and record the change in the manifest. A second sync must be a no-op:
// nothing left to clean up, and the same (still unresolved) conflict
// reported again rather than silently dropped or re-overwritten.
func TestSkillsSync_CleansShippedHashBackupsPreservesUserEditWritesPlumbNew(t *testing.T) {
	dir := t.TempDir()
	skills := embeddedSkills()
	if len(skills) < 2 {
		t.Fatal("need at least two embedded skills for this fixture")
	}
	target := skillsTestTarget("", dir)

	// First sync: install everything fresh, establishing the manifest.
	if _, results, cleanup := installSkillsFor(target, false); true {
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("initial install of %s: %v", r.name, r.err)
			}
		}
		if cleanup.err != nil {
			t.Fatalf("initial cleanup: %v", cleanup.err)
		}
	}

	backedUp := skills[0]
	shippedContent, err := os.ReadFile(filepath.Join(dir, backedUp.Name, "SKILL.md"))
	if err != nil {
		t.Fatalf("reading installed skill: %v", err)
	}

	// Two shipped-hash ".bak" directories: plumb's own content, backed up
	// more than once — the exact shape found in a live skills directory.
	bakNames := []string{
		backedUp.Name + ".20260819-074507.bak",
		backedUp.Name + ".20260819-083613.bak",
	}
	for _, name := range bakNames {
		bakDir := filepath.Join(dir, name)
		if err := os.MkdirAll(bakDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bakDir, "SKILL.md"), shippedContent, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A second embedded skill gets hand-edited by the user.
	edited := skills[1]
	editedPath := filepath.Join(dir, edited.Name, "SKILL.md")
	const userEdit = "hand-edited by the user\n"
	if err := os.WriteFile(editedPath, []byte(userEdit), 0o600); err != nil {
		t.Fatal(err)
	}

	// Sync again.
	dirOut, results, cleanup := installSkillsFor(target, false)
	if dirOut != dir {
		t.Fatalf("dir = %q, want %q", dirOut, dir)
	}
	if cleanup.err != nil {
		t.Fatalf("cleanup: %v", cleanup.err)
	}

	foundConflict := false
	for _, r := range results {
		if r.name != edited.Name {
			continue
		}
		foundConflict = true
		if r.err != nil {
			t.Errorf("edited skill reported an error: %v", r.err)
		}
		if r.action != skillActionConflict {
			t.Errorf("edited skill action = %q, want %q", r.action, skillActionConflict)
		}
	}
	if !foundConflict {
		t.Fatal("expected the edited skill to be reported")
	}

	// The user's file must be byte-for-byte untouched.
	gotEdited, err := os.ReadFile(editedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotEdited) != userEdit {
		t.Errorf("user edit was overwritten: got %q, want %q", gotEdited, userEdit)
	}

	// The proposed content must be offered for review as a plain file, never
	// a directory that a client's skill scan could mistake for another
	// "plumb-<x>" skill bundle.
	newFile := filepath.Join(dir, edited.Name+".plumb-new")
	fi, err := os.Stat(newFile)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", newFile, err)
	}
	if fi.IsDir() {
		t.Errorf("%s must be a file, not a directory", newFile)
	}
	gotNew, err := os.ReadFile(newFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := stampSkillContent(edited.Content); string(gotNew) != want {
		t.Errorf("plumb-new content = %q, want %q", gotNew, want)
	}

	// Both shipped-hash backups must be gone.
	for _, name := range bakNames {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, stat err=%v", name, err)
		}
	}
	if len(cleanup.removed) != len(bakNames) {
		t.Errorf("cleanup.removed = %v, want both shipped-hash backups", cleanup.removed)
	}
	if len(cleanup.kept) != 0 {
		t.Errorf("cleanup.kept = %v, want none — both backups matched a shipped hash", cleanup.kept)
	}

	// The manifest must record the change.
	manifest, err := loadSkillManifest(dir)
	if err != nil {
		t.Fatalf("loading manifest: %v", err)
	}
	if _, ok := manifest.Skills[backedUp.Name]; !ok {
		t.Errorf("manifest missing an entry for %s", backedUp.Name)
	}

	// Second sync is a no-op: nothing left to clean up, and the same
	// conflict reported again rather than silently dropped.
	_, results2, cleanup2 := installSkillsFor(target, false)
	if len(cleanup2.removed) != 0 || len(cleanup2.kept) != 0 {
		t.Errorf("second sync cleanup = %+v, want nothing left to report", cleanup2)
	}
	for _, r := range results2 {
		if r.err != nil {
			t.Errorf("second sync: %s reported an error: %v", r.name, r.err)
		}
		if r.name == edited.Name {
			// The proposal file already holds exactly this content from the
			// first sync, so the second run must not rewrite it — that is
			// what the "(proposal unchanged)" suffix asserts here.
			if want := skillActionConflict + conflictUnchangedSuffix; r.action != want {
				t.Errorf("second sync edited action = %q, want %q", r.action, want)
			}
			continue
		}
		if r.action != "unchanged" {
			t.Errorf("second sync %s action = %q, want %q", r.name, r.action, "unchanged")
		}
	}

	// The user's file is still untouched after the no-op re-run.
	gotEdited2, err := os.ReadFile(editedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotEdited2) != userEdit {
		t.Errorf("user edit was overwritten on the second sync: got %q, want %q", gotEdited2, userEdit)
	}
}

// TestSkillsSync_CheckWritesNothing pins `plumb skills sync --check`: every
// action is computed and reported exactly as a real sync would report it,
// but nothing reaches disk — no SKILL.md, no manifest, no ".plumb-new" file,
// and no backup is actually deleted.
func TestSkillsSync_CheckWritesNothing(t *testing.T) {
	dir := t.TempDir()
	target := skillsTestTarget("", dir)

	dirOut, results, cleanup := installSkillsFor(target, true)
	if dirOut != dir {
		t.Fatalf("dir = %q, want %q", dirOut, dir)
	}
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("%s: %v", r.name, r.err)
		}
		if r.action != "installed" {
			t.Errorf("%s: action = %q, want %q on an empty directory", r.name, r.action, "installed")
		}
	}
	if cleanup.err != nil {
		t.Fatalf("cleanup: %v", cleanup.err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("reading %s: %v", dir, err)
		}
		return
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("--check wrote to %s: %v", dir, names)
	}
}

// TestSkillsSync_CheckDoesNotDeleteBackups is --check's other half: a
// shipped-hash backup that a real sync would remove must survive a
// check-only run, while still being reported as something that would be
// removed.
func TestSkillsSync_CheckDoesNotDeleteBackups(t *testing.T) {
	dir := t.TempDir()
	target := skillsTestTarget("", dir)

	if _, results, _ := installSkillsFor(target, false); true {
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("initial install of %s: %v", r.name, r.err)
			}
		}
	}

	skill := embeddedSkills()[0]
	shipped, err := os.ReadFile(filepath.Join(dir, skill.Name, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	bakName := skill.Name + ".20260819-074507.bak"
	bakDir := filepath.Join(dir, bakName)
	if err := os.MkdirAll(bakDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bakDir, "SKILL.md"), shipped, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, cleanup := installSkillsFor(target, true)
	if len(cleanup.removed) != 1 || cleanup.removed[0] != bakName {
		t.Errorf("cleanup.removed = %v, want [%s] reported even though nothing was deleted", cleanup.removed, bakName)
	}
	if _, err := os.Stat(bakDir); err != nil {
		t.Errorf("--check deleted %s: %v", bakDir, err)
	}
}
