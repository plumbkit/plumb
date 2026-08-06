package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/tui"
)

// `plumb skills` is the whole skill surface, now that registration is
// config-only: a read-only status table on the bare command, and `sync` as the
// one writer. Keeping the writer off `plumb setup` means a config repair
// (`plumb setup --repair`) can never surprise a user by touching their skills
// directory, and a stale skill set has exactly one documented fix.

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Show the status of plumb's embedded skills per client",
	Long: `Show, for every client with a verified skills directory (claude-code,
codex, kimi-code), whether each embedded skill is installed, missing, or stale
relative to the copy compiled into this binary. Read-only.

` + "`plumb skills sync [client]`" + ` installs or refreshes the skills: every
registered skill-capable client, or just the named one. A client whose config
does not register plumb is skipped — sync never writes skill files for a client
that does not use plumb.`,
	Args: cobra.NoArgs,
	RunE: runSkillsStatus,
}

var skillsSyncCmd = &cobra.Command{
	Use:   "sync [client]",
	Short: "Install or refresh plumb's skills for registered clients",
	Long: `Install or refresh plumb's embedded skills in every skill-capable client
that registers plumb, or only in the named client. Changed skills are backed up
before being overwritten; unchanged ones are left alone. Naming a client that
does not register plumb is an error — run ` + "`plumb setup <client>`" + ` first.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSkillsSync,
}

func init() {
	skillsCmd.AddCommand(skillsSyncCmd)
}

// runSkillsStatus renders the per-client, per-skill freshness table. A
// skill-capable client that does not register plumb is shown as "not
// registered" rather than as a pile of missing skills, so the user understands
// why sync would skip it.
func runSkillsStatus(_ *cobra.Command, _ []string) error {
	tui.RebuildStyles()
	t := render.DottedTableBase(tui.SepStyle, tui.HintStyle).
		Headers("Client", "Skill", "Status", "Skills dir")
	for _, c := range skillCapableClients() {
		dir, err := c.skillsDirFn()
		if err != nil {
			t.Row(c.name, "—", "error", err.Error())
			continue
		}
		if !plumbRegisteredIn(c) {
			t.Row(c.name, "—", "not registered",
				render.ContractPath(dir)+" (sync skips it — run `plumb setup "+c.use+"`)")
			continue
		}
		for i, s := range embeddedSkills() {
			name, shown := c.name, render.ContractPath(dir)
			if i > 0 {
				name, shown = "", ""
			}
			t.Row(name, s.Name, skillStateAt(dir, s.Name, s.Content), shown)
		}
	}
	fmt.Println(t.Render())
	return nil
}

// runSkillsSync installs or refreshes skills: the named client when an argument
// is given, otherwise every registered skill-capable client. Per-skill errors
// are warnings, not fatal (see installAndPrintSkills) — a sync that partially
// failed still leaves every other skill correct.
func runSkillsSync(_ *cobra.Command, args []string) error {
	capable := skillCapableClients()
	if len(args) == 1 {
		t, ok := findSkillCapable(capable, args[0])
		if !ok {
			return fmt.Errorf("unknown client %q — skill-capable clients: %s", args[0], skillCapableNames(capable))
		}
		if !plumbRegisteredIn(t) {
			return fmt.Errorf("plumb is not registered in %s — run `plumb setup %s` first, then re-run `plumb skills sync %s`",
				t.name, t.use, t.use)
		}
		fmt.Println(skillSyncSummaryLine(t.name, installAndPrintSkills(t)))
		return nil
	}
	for _, c := range capable {
		if !plumbRegisteredIn(c) {
			fmt.Printf("Skipping %s — plumb is not registered (`plumb setup %s`).\n", c.name, c.use)
			continue
		}
		fmt.Println(skillSyncSummaryLine(c.name, installAndPrintSkills(c)))
	}
	return nil
}

// findSkillCapable resolves a sync argument against the capable set by command
// name (the spelling the user knows from `plumb setup <client>`).
func findSkillCapable(capable []setupTarget, name string) (setupTarget, bool) {
	for _, c := range capable {
		if c.use == name {
			return c, true
		}
	}
	return setupTarget{}, false
}

func skillCapableNames(capable []setupTarget) string {
	names := make([]string, 0, len(capable))
	for _, c := range capable {
		names = append(names, c.use)
	}
	return strings.Join(names, ", ")
}

// skillSyncTally is one client's sync outcome, aggregated for the summary
// line runSkillsSync prints per client.
type skillSyncTally struct {
	installed, updated, current, failed int
}

// installAndPrintSkills installs the embedded skills into t's skills directory,
// prints one line per skill that changed, and tallies the outcome. It is a
// no-op for a target with no skills directory.
//
// Errors are non-fatal by design: a failed skill install must not fail the rest
// of the sync, because one unwritable directory should not strand the others.
func installAndPrintSkills(t setupTarget) (tally skillSyncTally) {
	dir, results := installSkillsFor(t)
	for _, r := range results {
		switch {
		case r.err != nil:
			tally.failed++
			fmt.Fprintf(os.Stderr, "warning: installing skill %q: %v\n", r.name, r.err)
		case r.action == "unchanged":
			tally.current++
		default:
			if r.action == "installed" {
				tally.installed++
			} else {
				tally.updated++
			}
			fmt.Printf("Skill %-20s %s → %s\n", r.name, r.action, filepath.Join(dir, r.name, "SKILL.md"))
		}
	}
	return tally
}

// skillSyncSummaryLine renders one client's sync outcome as a single line. It
// is printed UNCONDITIONALLY: a writer command that succeeds silently is
// indistinguishable from a broken one, so "no output" may only ever mean the
// command did not run — never that it no-opped.
func skillSyncSummaryLine(client string, t skillSyncTally) string {
	total := t.installed + t.updated + t.current + t.failed
	if total == 0 {
		return client + ": nothing to sync"
	}
	if t.installed == 0 && t.updated == 0 && t.failed == 0 {
		return fmt.Sprintf("%s: %d %s current", client, t.current, textfmt.Plural(t.current, "skill", "skills"))
	}
	parts := make([]string, 0, 4)
	for _, p := range []struct {
		n    int
		word string
	}{
		{t.installed, "installed"},
		{t.updated, "updated"},
		{t.current, "current"},
		{t.failed, "failed"},
	} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.word))
		}
	}
	return fmt.Sprintf("%s: %d %s — %s", client, total, textfmt.Plural(total, "skill", "skills"), strings.Join(parts, ", "))
}

// The three states a skill file can be in relative to the embedded copy. The
// strings double as the status table's cell text.
const (
	skillStateInstalled = "installed"
	skillStateMissing   = "missing"
	skillStateStale     = "stale"
)

// skillStateAt classifies <dir>/<name>/SKILL.md against the embedded content.
// An unreadable file reports missing: sync's response to either is a write,
// which is where a genuine read error surfaces properly.
func skillStateAt(dir, name, content string) string {
	data, err := os.ReadFile(filepath.Join(dir, name, "SKILL.md"))
	switch {
	case err != nil:
		return skillStateMissing
	case string(data) != content:
		return skillStateStale
	default:
		return skillStateInstalled
	}
}

// printSkillsDriftHint is the post-registration pointer printed by the named
// `plumb setup <client>` commands: when a skill-capable client's skills are
// missing or stale it names the fix, and when they are current it says nothing.
// Detection only — registering a client never writes skill files.
func printSkillsDriftHint(t setupTarget) {
	if _, drifted := skillsDrift(t); drifted {
		fmt.Printf("\nSkills missing/outdated — run `plumb skills sync %s`.\n", t.use)
	}
}

// skillsDrift reports whether any of t's installed skills differ from the
// embedded source (missing or stale), along with the directory inspected. A
// client with no skill channel, or a skills directory that cannot be resolved,
// reports no drift — there is nothing `plumb skills sync` could do about it
// either.
func skillsDrift(t setupTarget) (dir string, drifted bool) {
	if t.skillsDirFn == nil {
		return "", false
	}
	dir, err := t.skillsDirFn()
	if err != nil {
		return "", false
	}
	for _, s := range embeddedSkills() {
		if skillStateAt(dir, s.Name, s.Content) != skillStateInstalled {
			return dir, true
		}
	}
	return dir, false
}

// skillDriftCounts tallies the non-current skills in dir, for the doctor
// grade's message (skillsDrift only answers whether there is anything to say).
func skillDriftCounts(dir string) (missing, stale int) {
	for _, s := range embeddedSkills() {
		switch skillStateAt(dir, s.Name, s.Content) {
		case skillStateMissing:
			missing++
		case skillStateStale:
			stale++
		}
	}
	return missing, stale
}

// plumbRegisteredIn reports whether t's canonical user-scoped config registers
// plumb. The gate is user scope deliberately: skills are user-scoped files, so
// a project-scope registration (Claude Code's .mcp.json) must not count — sync
// would otherwise write into ~/.claude/skills on the strength of a project file
// the next machine will never see. An absent config is simply "not registered";
// installedFn (Kimi Code's data-dir detection) does not apply because there is
// no file to extract a registration from.
func plumbRegisteredIn(t setupTarget) bool {
	cfgPath, err := t.pathFn()
	if err != nil {
		return false
	}
	if _, err := os.Stat(cfgPath); err != nil {
		return false
	}
	return clientHasPlumb(t, cfgPath)
}
