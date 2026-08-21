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
	err := runSkillsSync(false, []string{"not-a-client"})
	if err == nil {
		t.Fatal("an unknown client name must be an error")
	}
	for _, want := range []string{"not-a-client", "claude-code", "codex", "junie", "kimi-code", "zcode"} {
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

	err := runSkillsSync(false, []string{"codex"})
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
		if err := runSkillsSync(false, nil); err != nil {
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
// non-zero bucket, a failed install is never hidden, a conflict is called out
// as "needs review", and a backup-cleanup outcome appends as a trailing
// clause rather than displacing the skill tally.
func TestSkillSyncSummaryLine(t *testing.T) {
	cases := []struct {
		name    string
		tally   skillSyncTally
		cleanup skillCleanupReport
		want    string
	}{
		{"all current", skillSyncTally{current: 7}, skillCleanupReport{}, "Test: 7 skills current"},
		{"singular", skillSyncTally{current: 1}, skillCleanupReport{}, "Test: 1 skill current"},
		{"fresh install", skillSyncTally{installed: 7}, skillCleanupReport{}, "Test: 7 skills — 7 installed"},
		{"mixed", skillSyncTally{installed: 1, updated: 2, current: 4}, skillCleanupReport{}, "Test: 7 skills — 1 installed, 2 updated, 4 current"},
		{"failure is visible", skillSyncTally{current: 6, failed: 1}, skillCleanupReport{}, "Test: 7 skills — 6 current, 1 failed"},
		{"empty", skillSyncTally{}, skillCleanupReport{}, "Test: nothing to sync"},
		{"conflict is visible", skillSyncTally{current: 6, conflict: 1}, skillCleanupReport{}, "Test: 7 skills — 6 current, 1 needs review"},
		{"cleanup removed", skillSyncTally{current: 7}, skillCleanupReport{removed: []string{"a.bak", "b.bak"}}, "Test: 7 skills current; 2 shipped-hash backups removed"},
		{"cleanup kept", skillSyncTally{current: 7}, skillCleanupReport{kept: []string{"c.bak"}}, "Test: 7 skills current; 1 backup left for review (c.bak)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := skillSyncSummaryLine("Test", tc.tally, tc.cleanup); got != tc.want {
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
		if err := runSkillsSync(false, nil); err != nil {
			t.Errorf("first sync: %v", err)
		}
	})
	if want := "Codex: " + n + " skills — " + n + " installed"; !strings.Contains(out, want) {
		t.Errorf("first sync must summarise the installs (%q):\n%s", want, out)
	}

	out = captureStdout(t, func() {
		if err := runSkillsSync(false, nil); err != nil {
			t.Errorf("no-op re-sync: %v", err)
		}
	})
	if want := "Codex: " + n + " skills current"; !strings.Contains(out, want) {
		t.Errorf("a no-op re-sync must still say so (%q):\n%s", want, out)
	}
}

// TestRunSkillsSync_SummaryBlock pins the summary section's presentation: it
// sits one blank line below the table under a ● Summary heading, and every
// per-client line rides the ┊ gutter — the shared CLI section style — rather
// than printing bare directly after the table.
func TestRunSkillsSync_SummaryBlock(t *testing.T) {
	root := pointClientHomesAt(t)
	codexCfg := filepath.Join(root, "codex-home", "config.toml")
	if _, _, err := setupCodexInto(codexCfg, "/opt/plumb"); err != nil {
		t.Fatal(err)
	}

	n := strconv.Itoa(len(embeddedSkills()))
	out := captureStdout(t, func() {
		if err := runSkillsSync(false, nil); err != nil {
			t.Errorf("sync: %v", err)
		}
	})
	plain := ansiStripForCLITest(out)

	if !strings.Contains(plain, "\n\n● Summary\n\n") {
		t.Errorf("summary section must sit one blank line below the table, under its own heading with a blank line after it:\n%s", plain)
	}
	if want := "  ┊ Codex: " + n + " skills — " + n + " installed"; !strings.Contains(plain, want) {
		t.Errorf("summary lines must ride the ┊ gutter (%q):\n%s", want, plain)
	}
	if strings.Contains(plain, "\nCodex: ") {
		t.Errorf("a summary line escaped the gutter:\n%s", plain)
	}
	if want := "skills — " + n + " installed\n\nSkipping Claude Code"; !strings.Contains(plain, want) {
		t.Errorf("skip notes must sit one blank line below the summary block (%q):\n%s", want, plain)
	}
}

// TestRunSkillsStatus_UnregisteredBlock pins how the status table renders a
// skill-capable client that does not register plumb: the warn-coloured
// "unregistered" word, with the skip reason and its fix on their own rows
// under the skills directory rather than one long parenthesised tail on the
// directory cell.
func TestRunSkillsStatus_UnregisteredBlock(t *testing.T) {
	pointClientHomesAt(t)

	out := captureStdout(t, func() {
		if err := runSkillsStatus(nil, nil); err != nil {
			t.Errorf("status: %v", err)
		}
	})
	plain := ansiStripForCLITest(out)

	for _, want := range []string{
		"unregistered",
		"sync skips it, run:",
		"`plumb setup claude-code`",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("status table missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "not registered") || strings.Contains(plain, "(sync skips it") {
		t.Errorf("the old one-line parenthesised note must be gone:\n%s", plain)
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

	if _, results, _ := installSkillsFor(target, false); len(results) == 0 {
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

// TestSetupBulkFlags pins the bulk-flag surface on `plumb setup`: --all is the
// single bulk flag (register and repoint), and --repair and --install-missing
// survive one release as hidden, deprecated aliases of it.
func TestSetupBulkFlags(t *testing.T) {
	t.Run("--repair and --install-missing are hidden deprecated aliases", func(t *testing.T) {
		for _, name := range []string{"repair", "install-missing"} {
			f := setupCmd.Flags().Lookup(name)
			if f == nil {
				t.Fatalf("--%s must still parse during the deprecation window", name)
			}
			if !f.Hidden {
				t.Errorf("--%s must be hidden from help", name)
			}
			if f.Deprecated == "" {
				t.Errorf("--%s must carry a deprecation message", name)
			}
		}
	})

	t.Run("--all is registered", func(t *testing.T) {
		all := setupCmd.Flags().Lookup("all")
		if all == nil {
			t.Fatal("--all must exist")
		}
		if !strings.Contains(strings.ToLower(all.Usage), "register") {
			t.Errorf("--all's help must say it registers clients, got %q", all.Usage)
		}
	})

	t.Run("--no-skill is gone", func(t *testing.T) {
		for _, cmd := range []*cobra.Command{setupCmd, setupClaudeCodeCmd} {
			if cmd.Flags().Lookup("no-skill") != nil {
				t.Errorf("%s still carries --no-skill — skill installation moved to `plumb skills sync`, the opt-out went with it", cmd.Name())
			}
		}
	})

	t.Run("bulkRegistersMissing is true under --all and both aliases", func(t *testing.T) {
		t.Cleanup(func() { setupRepairFlag, setupAllFlag, setupInstallMissingFlag = false, false, false })
		for _, bulk := range []*bool{&setupAllFlag, &setupRepairFlag, &setupInstallMissingFlag} {
			*bulk = true
			if !bulkRegistersMissing() {
				t.Error("--repair and --install-missing are aliases of --all — every bulk flag must register missing clients")
			}
			*bulk = false
		}
	})
}

// TestPrintSetupAllSummary pins the trailing summary: a sweep that changed
// nothing says every installed client is already registered, a sweep that
// changed some counts them, and neither hints at --all — every bulk run
// already runs under it. A sweep that failed on a client must not claim every
// installed client is current: it could not read one, so it says so.
func TestPrintSetupAllSummary(t *testing.T) {
	out := captureStdout(t, func() { printSetupAllSummary(0, 0) })
	if !strings.Contains(out, "No changes") {
		t.Errorf("a no-change sweep must say so, got %q", out)
	}
	if strings.Contains(out, "plumb setup --all") {
		t.Errorf("the summary must not point at the flag the sweep already ran under, got %q", out)
	}

	out = captureStdout(t, func() { printSetupAllSummary(3, 0) })
	if !strings.Contains(out, "Updated 3 client(s)") {
		t.Errorf("a changing sweep must count its clients, got %q", out)
	}

	out = captureStdout(t, func() { printSetupAllSummary(0, 1) })
	if strings.Contains(out, "every installed client") {
		t.Errorf("a sweep that could not read a client must not vouch for every installed one, got %q", out)
	}
	if !strings.Contains(out, "No changes") {
		t.Errorf("a no-change sweep must still say so when a client failed, got %q", out)
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

	if _, results, _ := installSkillsFor(target, false); len(results) == 0 {
		t.Fatal("expected the skills to install")
	}
	if _, ok := skillFreshnessResult(target); ok {
		t.Error("current skills must produce no doctor line")
	}
}
