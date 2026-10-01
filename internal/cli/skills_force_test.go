package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/render"
)

// forceTestEdit stands in for a user's hand edit of a shipped SKILL.md.
const forceTestEdit = "hand-edited by the user\n"

// editedCodexSkill registers Codex under a temp HOME, installs every embedded
// skill, then overwrites the first skill's SKILL.md with a user edit. It
// returns the Codex skills directory and the edited skill.
func editedCodexSkill(t *testing.T) (skillsDir string, skill embeddedSkill) {
	t.Helper()
	root := pointClientHomesAt(t)
	if _, _, err := setupCodexInto(filepath.Join(root, "codex-home", "config.toml"), "/opt/plumb"); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		if err := runSkillsSync(false, nil); err != nil {
			t.Fatalf("initial sync: %v", err)
		}
	})
	skillsDir = filepath.Join(root, "codex-home", "skills")
	skill = embeddedSkills()[0]
	if err := os.WriteFile(filepath.Join(skillsDir, skill.Name, "SKILL.md"), []byte(forceTestEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	return skillsDir, skill
}

func skillBackups(t *testing.T, skillsDir, name string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(skillsDir, name, "SKILL.md.*.bak"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestRunSkillsSync_ForceReplacesEditedSkillAndReportsBackup is --force's
// contract, end to end through runSkillsSync: an edited skill is saved as a
// .bak, overwritten with the shipped copy, its stale ".plumb-new" proposal is
// removed, and the report names the backup rather than saying plain "updated".
func TestRunSkillsSync_ForceReplacesEditedSkillAndReportsBackup(t *testing.T) {
	skillsDir, skill := editedCodexSkill(t)
	skillPath := filepath.Join(skillsDir, skill.Name, "SKILL.md")
	proposal := filepath.Join(skillsDir, skill.Name+".plumb-new")

	// An ordinary sync first, so a ".plumb-new" is left behind to go stale.
	captureStdout(t, func() {
		if err := runSkillsSync(false, nil); err != nil {
			t.Fatalf("sync without force: %v", err)
		}
	})
	if !fileExists(proposal) {
		t.Fatalf("fixture: expected %s from the unforced sync", proposal)
	}

	out := captureStdout(t, func() {
		if err := runSkillsSync(false, nil, true); err != nil {
			t.Errorf("forced sync: %v", err)
		}
	})
	plain := ansiStripForCLITest(out)

	if got, want := readFileString(t, skillPath), stampSkillContent(skill.Content); got != want {
		t.Errorf("SKILL.md was not replaced by the shipped copy:\n%q", got)
	}
	backups := skillBackups(t, skillsDir, skill.Name)
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	if got := readFileString(t, backups[0]); got != forceTestEdit {
		t.Errorf("backup holds %q, want the user's edit %q", got, forceTestEdit)
	}
	if fileExists(proposal) {
		t.Errorf("stale proposal %s was left behind", proposal)
	}
	if want := "backup: " + render.ContractPath(backups[0]); !strings.Contains(plain, want) {
		t.Errorf("report must say where the backup went (%q):\n%s", want, plain)
	}
	if !strings.Contains(plain, "replaced") || !strings.Contains(plain, "1 replaced") {
		t.Errorf("report must call the skill replaced, in the row and the summary:\n%s", plain)
	}
	if strings.Contains(plain, "conflict") || strings.Contains(plain, "needs review") {
		t.Errorf("a forced sync leaves no conflict to report:\n%s", plain)
	}

	// The manifest now records the shipped copy, so a re-run is quiet.
	again := ansiStripForCLITest(captureStdout(t, func() {
		if err := runSkillsSync(false, nil, true); err != nil {
			t.Errorf("re-run: %v", err)
		}
	}))
	if strings.Contains(again, "replaced") {
		t.Errorf("a second forced sync must not replace again:\n%s", again)
	}
	if n := len(skillBackups(t, skillsDir, skill.Name)); n != 1 {
		t.Errorf("a second forced sync wrote another backup: %d", n)
	}
}

// TestRunSkillsSync_WithoutForceLeavesConflictAlone pins the default: an
// edited skill is untouched, no backup is made, the proposal is written, and
// the report points at --force.
func TestRunSkillsSync_WithoutForceLeavesConflictAlone(t *testing.T) {
	skillsDir, skill := editedCodexSkill(t)

	out := captureStdout(t, func() {
		if err := runSkillsSync(false, nil); err != nil {
			t.Errorf("sync: %v", err)
		}
	})
	plain := ansiStripForCLITest(out)

	if got := readFileString(t, filepath.Join(skillsDir, skill.Name, "SKILL.md")); got != forceTestEdit {
		t.Errorf("the user's edit was changed without --force: %q", got)
	}
	if backups := skillBackups(t, skillsDir, skill.Name); len(backups) != 0 {
		t.Errorf("no backup is expected without --force: %v", backups)
	}
	if !fileExists(filepath.Join(skillsDir, skill.Name+".plumb-new")) {
		t.Error("the proposal must be written for review")
	}
	for _, want := range []string{"conflict", "1 needs review", "plumb skills sync --force codex"} {
		if !strings.Contains(plain, want) {
			t.Errorf("report missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "replaced") {
		t.Errorf("nothing may be reported replaced without --force:\n%s", plain)
	}
}

// TestInstallSkillsFor_ForceRemovesStaleProposal pins that forcing over a
// conflict deletes that skill's "<name>.plumb-new", and only that skill's.
func TestInstallSkillsFor_ForceRemovesStaleProposal(t *testing.T) {
	dir := t.TempDir()
	target := skillsTestTarget("", dir)
	skills := embeddedSkills()
	if len(skills) < 2 {
		t.Fatal("need two embedded skills for this fixture")
	}
	forced, bystander := skills[0], skills[1]
	installSkillsFor(target, false)
	if err := os.WriteFile(filepath.Join(dir, forced.Name, "SKILL.md"), []byte(forceTestEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	installSkillsFor(target, false) // conflict: writes the proposal
	forcedProposal := filepath.Join(dir, forced.Name+".plumb-new")
	if !fileExists(forcedProposal) {
		t.Fatalf("fixture: no proposal for %s", forced.Name)
	}
	// A proposal beside a skill that is not being replaced is not this
	// sync's to remove.
	bystanderProposal := filepath.Join(dir, bystander.Name+".plumb-new")
	if err := os.WriteFile(bystanderProposal, []byte("notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, results, _ := installSkillsFor(target, false, true)
	for _, r := range results {
		if r.err != nil {
			t.Errorf("%s: %v", r.name, r.err)
		}
		if r.name == forced.Name && !strings.HasPrefix(r.action, replacedBackupPrefix) {
			t.Errorf("forced skill action = %q, want a %q action", r.action, skillActionReplaced)
		}
	}
	if fileExists(forcedProposal) {
		t.Errorf("proposal for %s survived a forced replacement", forced.Name)
	}
	if !fileExists(bystanderProposal) {
		t.Errorf("proposal for %s was removed, but that skill was not replaced", bystander.Name)
	}
}

// TestInstallSkillsFor_ForceDryRunWritesNothing pins --check --force: the
// replacement is reported but the edit, the proposal and the directory are
// left exactly as they were.
func TestInstallSkillsFor_ForceDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	target := skillsTestTarget("", dir)
	skill := embeddedSkills()[0]
	installSkillsFor(target, false)
	skillPath := filepath.Join(dir, skill.Name, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(forceTestEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	installSkillsFor(target, false) // leaves a proposal

	_, results, _ := installSkillsFor(target, true, true)
	var got string
	for _, r := range results {
		if r.name == skill.Name {
			got = r.action
		}
	}
	if got != skillActionReplaced {
		t.Errorf("dry-run action = %q, want %q", got, skillActionReplaced)
	}
	if readFileString(t, skillPath) != forceTestEdit {
		t.Error("--check --force overwrote the edited skill")
	}
	if len(skillBackups(t, dir, skill.Name)) != 0 {
		t.Error("--check --force wrote a backup")
	}
	if !fileExists(filepath.Join(dir, skill.Name+".plumb-new")) {
		t.Error("--check --force removed the proposal")
	}
}

// TestSkillTables_PipedOutputKeepsNaturalWidth pins that a table written to a
// pipe is not hard-wrapped at 80 columns: a skills directory longer than that
// stays whole on its row, as grep and awk expect.
func TestSkillTables_PipedOutputKeepsNaturalWidth(t *testing.T) {
	root := pointClientHomesAt(t)
	home := filepath.Join(root, strings.Repeat("d", 70), "codex-home")
	t.Setenv("CODEX_HOME", home)
	if _, _, err := setupCodexInto(filepath.Join(home, "config.toml"), "/opt/plumb"); err != nil {
		t.Fatal(err)
	}
	long := render.ContractPath(filepath.Join(home, "skills"))
	if len(long) <= 80 {
		t.Fatalf("fixture: %q is not wider than 80", long)
	}

	for name, run := range map[string]func() error{
		"sync":   func() error { return runSkillsSync(false, nil) },
		"status": func() error { return runSkillsStatus(nil, nil) },
	} {
		out := captureStdout(t, func() {
			if err := run(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		})
		if plain := ansiStripForCLITest(out); !strings.Contains(plain, long) {
			t.Errorf("%s: piped table wrapped the %d-column path:\n%s", name, len(long), plain)
		}
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if got := tableWidth(w); got != 0 {
		t.Errorf("tableWidth(pipe) = %d, want 0 (natural width)", got)
	}
}
