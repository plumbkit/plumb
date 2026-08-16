package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// skillsTestTarget builds a skill-capable target pointed at temp config and
// skills dirs, with Claude Desktop's plain mcpServers JSON shape for the
// registration side.
func skillsTestTarget(cfgPath, skillsDir string) setupTarget {
	return setupTarget{
		use: "test-client", name: "Test Client",
		pathFn:      func() (string, error) { return cfgPath, nil },
		intoFn:      setupClaudeDesktopInto,
		extractFn:   claudeDesktopCommandExtractor,
		skillsDirFn: func() (string, error) { return skillsDir, nil },
	}
}

// pinVersion sets the build version for a marker-comparison test and
// restores it on cleanup.
func pinVersion(t *testing.T, v string) {
	t.Helper()
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

// TestSkillStateAt pins the three-way classification the status table, the
// post-registration hint, and the doctor grade all share — including the
// provenance wording: the marker is stripped before the content comparison,
// so a version bump alone is not drift, and a differing skill with no marker
// reports its provenance as unknown rather than inventing one.
func TestSkillStateAt(t *testing.T) {
	const content = "# Skill\n"
	dir := t.TempDir()

	cases := []struct {
		name  string
		write string // "" leaves the file absent
		want  string
	}{
		{"absent is missing", "", skillStateMissing},
		{"identical is installed", content, skillStateInstalled},
		{"stamped identical is installed", stampSkillContent(content), skillStateInstalled},
		{"different without a marker is stale, unknown version", "old content\n", skillStateStale + " (unknown version / hand-edited)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skillDir := filepath.Join(dir, "demo")
			if err := os.RemoveAll(skillDir); err != nil {
				t.Fatal(err)
			}
			if tc.write != "" {
				if err := os.MkdirAll(skillDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(tc.write), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := skillStateAt(dir, "demo", content, nil); got != tc.want {
				t.Errorf("skillStateAt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSkillStateAt_Provenance pins the marker-aware wording for a stale
// skill: a strictly older installing version is named, an equal or newer (or
// unparseable) marker falls back to the plain form because the version
// cannot explain the drift, and identical content with any marker stays
// "installed" — the marker is metadata, never content.
func TestSkillStateAt_Provenance(t *testing.T) {
	pinVersion(t, "0.16.3")
	const content = "# Skill\n"
	dir := t.TempDir()

	cases := []struct {
		name  string
		write string
		want  string
	}{
		{"older marker is named", "<!-- plumb: 0.15.1 -->\nold\n", "stale (installed by 0.15.1)"},
		{"equal marker stays plain", "<!-- plumb: 0.16.3 -->\nold\n", skillStateStale},
		{"newer marker stays plain", "<!-- plumb: 0.17.0 -->\nold\n", skillStateStale},
		{"unparseable marker stays plain", "<!-- plumb: dev -->\nold\n", skillStateStale},
		{"old marker with identical content is installed", "<!-- plumb: 0.15.1 -->\n# Skill\n", skillStateInstalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skillDir := filepath.Join(dir, "demo")
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(tc.write), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := skillStateAt(dir, "demo", content, nil); got != tc.want {
				t.Errorf("skillStateAt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPlumbRegisteredIn pins the sync gate: absent config and config without a
// plumb entry are both "not registered", and only a real registration lets sync
// write into the client's skills directory.
func TestPlumbRegisteredIn(t *testing.T) {
	t.Run("absent config", func(t *testing.T) {
		cfg := filepath.Join(t.TempDir(), "mcp.json") // never created
		if plumbRegisteredIn(skillsTestTarget(cfg, t.TempDir())) {
			t.Error("an absent config must not count as registered")
		}
	})
	t.Run("config without a plumb entry", func(t *testing.T) {
		cfg := filepath.Join(t.TempDir(), "mcp.json")
		if err := os.WriteFile(cfg, []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if plumbRegisteredIn(skillsTestTarget(cfg, t.TempDir())) {
			t.Error("a config with no plumb entry must not count as registered")
		}
	})
	t.Run("config registering plumb", func(t *testing.T) {
		cfg := filepath.Join(t.TempDir(), "mcp.json")
		if _, _, err := setupClaudeDesktopInto(cfg, "/opt/plumb"); err != nil {
			t.Fatal(err)
		}
		if !plumbRegisteredIn(skillsTestTarget(cfg, t.TempDir())) {
			t.Error("a registered client must pass the gate")
		}
	})
}

// TestRunSkillsSync_UnknownClientIsAUsageError pins the named form's
// validation: an unrecognised name fails and lists the valid ones, so a typo
// cannot silently sync nothing.
func TestRunSkillsSync_UnknownClientIsAUsageError(t *testing.T) {
	err := runSkillsSync(nil, []string{"not-a-client"})
	if err == nil {
		t.Fatal("an unknown client name must be an error")
	}
	for _, want := range []string{"not-a-client", "claude-code", "codex", "kimi-code", "zcode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q: %v", want, err)
		}
	}
}

// TestRunSkillsSync_NamedUnregisteredClientErrors pins the named-form gate:
// naming an unregistered client points at `plumb setup <client>` and writes
// nothing, rather than failing silently the way the sweep's skip does.
func TestRunSkillsSync_NamedUnregisteredClientErrors(t *testing.T) {
	root := pointClientHomesAt(t)

	err := runSkillsSync(nil, []string{"codex"})
	if err == nil {
		t.Fatal("syncing an unregistered client by name must be an error")
	}
	if !strings.Contains(err.Error(), "plumb setup codex") {
		t.Errorf("error must point at registration: %v", err)
	}
	assertNoSkillsWritten(t, root)
}

// TestRunSkillsSync_SweepInstallsForRegisteredClientsOnly is the sweep's
// contract: registered skill-capable clients get every embedded skill;
// unregistered ones are skipped with a message, and nothing lands in their
// trees.
func TestRunSkillsSync_SweepInstallsForRegisteredClientsOnly(t *testing.T) {
	root := pointClientHomesAt(t)

	// Register codex only; claude-code and kimi-code stay unregistered.
	codexCfg := filepath.Join(root, "codex-home", "config.toml")
	if _, _, err := setupCodexInto(codexCfg, "/opt/plumb"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runSkillsSync(nil, nil); err != nil {
			t.Errorf("sync sweep: %v", err)
		}
	})

	for _, s := range embeddedSkills() {
		if _, err := os.Stat(filepath.Join(root, "codex-home", "skills", s.Name, "SKILL.md")); err != nil {
			t.Errorf("skill %q not synced for the registered client: %v", s.Name, err)
		}
	}
	for _, want := range []string{"Skipping Claude Code", "Skipping Kimi Code"} {
		if !strings.Contains(out, want) {
			t.Errorf("sweep output missing %q:\n%s", want, out)
		}
	}
	for _, p := range []string{
		filepath.Join(root, ".claude", "skills"),
		filepath.Join(root, "kimi-home", "skills"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("an unregistered client's skills dir was written: %s", p)
		}
	}
}

// TestSkillSyncSummaryLine pins the per-client summary's shape: an
// all-current client collapses to the short form, a mixed outcome lists every
// non-zero bucket, and a failed install is never hidden.
func TestSkillSyncSummaryLine(t *testing.T) {
	cases := []struct {
		name  string
		tally skillSyncTally
		want  string
	}{
		{"all current", skillSyncTally{current: 7}, "Test: 7 skills current"},
		{"singular", skillSyncTally{current: 1}, "Test: 1 skill current"},
		{"fresh install", skillSyncTally{installed: 7}, "Test: 7 skills — 7 installed"},
		{"mixed", skillSyncTally{installed: 1, updated: 2, current: 4}, "Test: 7 skills — 1 installed, 2 updated, 4 current"},
		{"failure is visible", skillSyncTally{current: 6, failed: 1}, "Test: 7 skills — 6 current, 1 failed"},
		{"empty", skillSyncTally{}, "Test: nothing to sync"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := skillSyncSummaryLine("Test", tc.tally); got != tc.want {
				t.Errorf("skillSyncSummaryLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunSkillsSync_NeverSilent pins the property the summary line exists
// for: a sync over a registered client always says what happened — on the
// first run that installs, and on the no-op re-run that changes nothing.
func TestRunSkillsSync_NeverSilent(t *testing.T) {
	root := pointClientHomesAt(t)
	codexCfg := filepath.Join(root, "codex-home", "config.toml")
	if _, _, err := setupCodexInto(codexCfg, "/opt/plumb"); err != nil {
		t.Fatal(err)
	}

	n := strconv.Itoa(len(embeddedSkills()))
	out := captureStdout(t, func() {
		if err := runSkillsSync(nil, nil); err != nil {
			t.Errorf("first sync: %v", err)
		}
	})
	if want := "Codex: " + n + " skills — " + n + " installed"; !strings.Contains(out, want) {
		t.Errorf("first sync must summarise the installs (%q):\n%s", want, out)
	}

	out = captureStdout(t, func() {
		if err := runSkillsSync(nil, nil); err != nil {
			t.Errorf("no-op re-sync: %v", err)
		}
	})
	if want := "Codex: " + n + " skills current"; !strings.Contains(out, want) {
		t.Errorf("a no-op re-sync must still say so (%q):\n%s", want, out)
	}
}

// TestPrintSkillsDriftHint pins the post-registration hint to its trigger: it
// fires on missing or stale skills with the sync command named, and stays
// silent when the skills are current or the client has no skill channel —
// registration output must not nag a machine that is already in line.
func TestPrintSkillsDriftHint(t *testing.T) {
	dir := t.TempDir()
	target := setupTarget{
		use: "test-client", name: "Test Client",
		skillsDirFn: func() (string, error) { return dir, nil },
	}

	out := captureStdout(t, func() { printSkillsDriftHint(target) })
	if !strings.Contains(out, "plumb skills sync test-client") {
		t.Errorf("missing skills must trigger the hint, got %q", out)
	}

	if _, results := installSkillsFor(target); len(results) == 0 {
		t.Fatal("expected the skills to install for the test target")
	}
	out = captureStdout(t, func() { printSkillsDriftHint(target) })
	if strings.Contains(out, "plumb skills sync") {
		t.Errorf("current skills must stay silent, got %q", out)
	}

	out = captureStdout(t, func() { printSkillsDriftHint(setupTarget{name: "No Channel"}) })
	if out != "" {
		t.Errorf("a client with no skill channel must print nothing, got %q", out)
	}
}

// TestSetupBulkFlags pins the new bulk-flag surface on `plumb setup`: --repair
// is the repoint-only sweep, --all additionally registers missing clients, and
// --install-missing survives one release as a hidden, deprecated alias.
func TestSetupBulkFlags(t *testing.T) {
	t.Run("--install-missing is a hidden deprecated alias", func(t *testing.T) {
		f := setupCmd.Flags().Lookup("install-missing")
		if f == nil {
			t.Fatal("--install-missing must still parse during the deprecation window")
		}
		if !f.Hidden {
			t.Error("--install-missing must be hidden from help")
		}
		if f.Deprecated == "" {
			t.Error("--install-missing must carry a deprecation message")
		}
	})

	t.Run("--repair and --all are registered", func(t *testing.T) {
		if setupCmd.Flags().Lookup("repair") == nil {
			t.Fatal("--repair must exist — it is what doctor's repair advice points at")
		}
		all := setupCmd.Flags().Lookup("all")
		if all == nil {
			t.Fatal("--all must exist")
		}
		if !strings.Contains(all.Usage, "register") {
			t.Errorf("--all's help must say it registers missing clients, got %q", all.Usage)
		}
	})

	t.Run("--no-skill is gone", func(t *testing.T) {
		for _, cmd := range []*cobra.Command{setupCmd, setupClaudeCodeCmd} {
			if cmd.Flags().Lookup("no-skill") != nil {
				t.Errorf("%s still carries --no-skill — skill installation moved to `plumb skills sync`, the opt-out went with it", cmd.Name())
			}
		}
	})

	t.Run("bulkRegistersMissing follows --all and the alias, not --repair", func(t *testing.T) {
		t.Cleanup(func() { setupRepairFlag, setupAllFlag, setupInstallMissingFlag = false, false, false })
		setupRepairFlag = true
		if bulkRegistersMissing() {
			t.Error("--repair must not register missing clients")
		}
		setupAllFlag = true
		if !bulkRegistersMissing() {
			t.Error("--all must register missing clients")
		}
		setupAllFlag = false
		setupInstallMissingFlag = true
		if !bulkRegistersMissing() {
			t.Error("--install-missing must keep its old behaviour during the deprecation window")
		}
	})
}

// TestPrintSetupAllSummary_PointsRepairAtAll pins the trailing hint: a
// repair-only run that finds installed-but-unregistered clients points at
// --all, and a run that already registers them says nothing about it.
func TestPrintSetupAllSummary_PointsRepairAtAll(t *testing.T) {
	t.Cleanup(func() { setupRepairFlag, setupAllFlag, setupInstallMissingFlag = false, false, false })

	setupRepairFlag = true
	out := captureStdout(t, func() { printSetupAllSummary(0, 2) })
	if !strings.Contains(out, "plumb setup --all") {
		t.Errorf("a repair-only run with unregistered clients must point at --all, got %q", out)
	}

	setupAllFlag = true
	out = captureStdout(t, func() { printSetupAllSummary(0, 2) })
	if strings.Contains(out, "run `plumb setup --all`") {
		t.Errorf("a registering run must not hint at the flag it already is, got %q", out)
	}

	setupAllFlag = false
	out = captureStdout(t, func() { printSetupAllSummary(0, 0) })
	if strings.Contains(out, "run `plumb setup --all`") {
		t.Errorf("no unregistered clients, no hint, got %q", out)
	}
}

// TestDoctorSeesSkillDrift is the cross-check that the doctor grade and the
// status table read the same classification: drift the doctor reports must be
// exactly what skillStateAt sees, so a fix (`plumb skills sync`) cannot leave
// doctor still complaining.
func TestDoctorSeesSkillDrift(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "mcp.json")
	skillsDir := filepath.Join(dir, "skills")
	target := skillsTestTarget(cfg, skillsDir)
	if _, _, err := setupClaudeDesktopInto(cfg, "/opt/plumb"); err != nil {
		t.Fatal(err)
	}

	res, ok := skillFreshnessResult(target)
	if !ok {
		t.Fatal("a registered client with no skills installed must produce a line")
	}
	if !strings.Contains(res.detail, strconv.Itoa(len(embeddedSkills()))+" skill(s) missing") {
		t.Errorf("detail should count every embedded skill as missing: %q", res.detail)
	}

	if _, results := installSkillsFor(target); len(results) == 0 {
		t.Fatal("expected the skills to install")
	}
	if _, ok := skillFreshnessResult(target); ok {
		t.Error("current skills must produce no doctor line")
	}
}
