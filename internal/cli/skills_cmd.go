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
codex, junie, kimi-code, zcode), whether each embedded skill is installed, missing, or stale
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
that registers plumb, or only in the named client. A skill plumb shipped
before is replaced in place — no backup — because its content hash is on
record (` + "`.plumb/skills-manifest.json`" + ` in the skills directory); a skill the
user has edited is left untouched, with the proposed content written to a
"<name>.plumb-new" file alongside it for review. Directory-level ".bak"
backups from a prior run whose content is provably plumb's own (a recorded
shipped hash) are cleaned up automatically; any others are left for manual
review. Naming a client that does not register plumb is an error — run ` +
		"`plumb setup <client>`" + ` first.

` + "`--check`" + ` reports every action sync would take — including which
backups would be cleaned up — without writing anything.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("check")
		return runSkillsSync(dryRun, args)
	},
}

func init() {
	skillsSyncCmd.Flags().Bool("check", false, "List drift without writing")
	skillsCmd.AddCommand(skillsSyncCmd)
}

// runSkillsStatus renders the per-client, per-skill freshness table. A
// skill-capable client that does not register plumb is shown as "unregistered"
// rather than as a pile of missing skills, so the user understands why sync
// would skip it — with the skip reason and its fix on their own rows under
// the skills directory, not one long parenthesised tail on the directory
// cell (the widest cell in the table, which stretched every dotted rule).
func runSkillsStatus(_ *cobra.Command, _ []string) error {
	tui.RebuildStyles()
	t := render.NewGroupedTable(tui.SepStyle, tui.HintStyle, "Client", "Skill", "Status", "Skills dir")
	for _, c := range skillCapableClients() {
		t.NextGroup()
		dir, err := c.skillsDirFn()
		if err != nil {
			t.Row(c.name, "—", statusStyle("error").Render("error"), err.Error())
			continue
		}
		if !plumbRegisteredIn(c) {
			t.Row(c.name, "—", statusStyle("unregistered").Render("unregistered"), render.ContractPath(dir))
			t.Row("", "", "", "sync skips it, run:")
			t.Row("", "", "", "`plumb setup "+c.use+"`")
			continue
		}
		for i, s := range embeddedSkills() {
			name, shown := c.name, render.ContractPath(dir)
			if i > 0 {
				name, shown = "", ""
			}
			state := skillStateAt(dir, s.Name, s.Content, s.References)
			t.Row(name, s.Name, statusStyle(state).Render(state), shown)
		}
	}
	fmt.Println(t.Render())
	return nil
}

// runSkillsSync installs or refreshes skills: the named client when an argument
// is given, otherwise every registered skill-capable client. The report is the
// same grouped table `plumb skills` shows — one group per client, so every
// skill row reads under the client it belongs to — with the action taken in
// the status cell, a ● Summary section collecting one ┊-guttered line per
// client, and a muted skip note per unregistered client in the sweep.
// Per-skill errors are rows with an error status, not fatal (see
// syncClientGroup) — a sync that partially failed still leaves every other
// skill correct.
func runSkillsSync(dryRun bool, args []string) error {
	tui.RebuildStyles()
	capable := skillCapableClients()
	var targets []setupTarget
	var skips []string
	if len(args) == 1 {
		target, ok := findSkillCapable(capable, args[0])
		if !ok {
			return fmt.Errorf("unknown client %q — skill-capable clients: %s", args[0], skillCapableNames(capable))
		}
		if !plumbRegisteredIn(target) {
			return fmt.Errorf("plumb is not registered in %s — run `plumb setup %s` first, then re-run `plumb skills sync %s`",
				target.name, target.use, target.use)
		}
		targets = []setupTarget{target}
	} else {
		for _, c := range capable {
			if !plumbRegisteredIn(c) {
				skips = append(skips, fmt.Sprintf("Skipping %s — plumb is not registered (`plumb setup %s`).", c.name, c.use))
				continue
			}
			targets = append(targets, c)
		}
	}

	if dryRun {
		fmt.Println(tui.HintStyle.Render("● Check only — no changes will be written"))
		fmt.Println()
	}

	t := render.NewGroupedTable(tui.SepStyle, tui.HintStyle, "Client", "Skill", "Status", "Skills dir")
	var summaries []string
	for _, target := range targets {
		syncClientGroup(t, &summaries, target, dryRun)
	}
	fmt.Println(t.Render())
	if len(summaries) > 0 {
		fmt.Println()
		fmt.Println(tui.HintStyle.Render("● Summary"))
		fmt.Println()
		for _, s := range summaries {
			fmt.Printf("  %s %s\n", tui.SepStyle.Render("┊"), s)
		}
	}
	if len(skips) > 0 {
		fmt.Println()
	}
	for _, s := range skips {
		fmt.Println(tui.MutedStyle.Render(s))
	}
	return nil
}

// syncClientGroup installs one client's skills and appends their grouped-table
// rows plus the client's summary line. The status cell carries the action
// taken — "current" for an unchanged skill, the summary line's vocabulary —
// and a failed install shows "error" with the reason in place of the dir:
// errors stay visible without failing the sync. A conflict (skill the user
// edited) shows the ".plumb-new" review file in place of the dir, so the
// table itself says where to look — no separate report needed. Backup
// cleanup is appended to the client's summary line rather than given its own
// row: it is not a per-skill outcome, and a table row with no matching skill
// name would look like a bug.
func syncClientGroup(t *render.GroupedTable, summaries *[]string, target setupTarget, dryRun bool) {
	dir, results, cleanup := installSkillsFor(target, dryRun)
	var tally skillSyncTally
	t.NextGroup()
	for i, r := range results {
		name, shown := target.name, render.ContractPath(dir)
		if i > 0 {
			name, shown = "", ""
		}
		status := r.action
		switch {
		case r.err != nil:
			status = "error"
			shown = r.err.Error()
			tally.failed++
		case r.action == "unchanged":
			status = "current"
			tally.current++
		case r.action == "installed":
			tally.installed++
		case strings.HasPrefix(r.action, skillActionConflict):
			status = skillActionConflict
			word := "proposal updated"
			if strings.HasSuffix(r.action, conflictUnchangedSuffix) {
				word = "proposal unchanged"
			}
			shown = render.ContractPath(filepath.Join(dir, r.name+".plumb-new")) + " (differs from the shipped version — user-edited or predates the manifest — " + word + ", review and merge)"
			tally.conflict++
		default:
			tally.updated++
		}
		t.Row(name, r.name, statusStyle(status).Render(status), shown)
	}
	*summaries = append(*summaries, skillSyncSummaryLine(target.name, tally, cleanup))
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
	installed, updated, current, failed, conflict int
}

// skillSyncSummaryLine renders one client's sync outcome as a single line,
// plus the cleanup pass's outcome when it did anything. It is printed
// UNCONDITIONALLY: a writer command that succeeds silently is
// indistinguishable from a broken one, so "no output" may only ever mean the
// command did not run — never that it no-opped.
func skillSyncSummaryLine(client string, t skillSyncTally, cleanup skillCleanupReport) string {
	total := t.installed + t.updated + t.current + t.failed + t.conflict
	line := client + ": nothing to sync"
	switch {
	case total == 0:
		// line already set.
	case t.installed == 0 && t.updated == 0 && t.failed == 0 && t.conflict == 0:
		line = fmt.Sprintf("%s: %d %s current", client, t.current, textfmt.Plural(t.current, "skill", "skills"))
	default:
		parts := make([]string, 0, 5)
		for _, p := range []struct {
			n    int
			word string
		}{
			{t.installed, "installed"},
			{t.updated, "updated"},
			{t.current, "current"},
			{t.conflict, "needs review"},
			{t.failed, "failed"},
		} {
			if p.n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", p.n, p.word))
			}
		}
		line = fmt.Sprintf("%s: %d %s — %s", client, total, textfmt.Plural(total, "skill", "skills"), strings.Join(parts, ", "))
	}
	return line + skillCleanupSuffix(cleanup)
}

// skillCleanupSuffix renders the backup-cleanup outcome as a trailing clause
// on the summary line, empty when there was nothing to report — so a run
// with no ".bak" litter reads exactly as it did before this pass existed.
func skillCleanupSuffix(cleanup skillCleanupReport) string {
	switch {
	case cleanup.err != nil:
		return fmt.Sprintf("; backup cleanup failed: %s", cleanup.err)
	case len(cleanup.removed) == 0 && len(cleanup.kept) == 0:
		return ""
	}
	var parts []string
	if n := len(cleanup.removed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d shipped-hash %s removed", n, textfmt.Plural(n, "backup", "backups")))
	}
	if n := len(cleanup.kept); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s left for review (%s)", n, textfmt.Plural(n, "backup", "backups"), strings.Join(cleanup.kept, ", ")))
	}
	return "; " + strings.Join(parts, ", ")
}

// The three states a skill file can be in relative to the embedded copy. The
// strings double as the status table's cell text; a stale state may carry a
// provenance suffix (see skillStateAt), so code that classifies rather than
// prints must match the prefix, not the whole string.
const (
	skillStateInstalled = "installed"
	skillStateMissing   = "missing"
	skillStateStale     = "stale"
)

// skillStateAt classifies <dir>/<name>/SKILL.md against the embedded content.
// An unreadable file reports missing: sync's response to either is a write,
// which is where a genuine read error surfaces properly. The comparison
// strips plumb's provenance marker first, so a version bump alone (content
// identical) still reads "installed" — the marker is metadata, not content.
// A differing skill reports its provenance when there is any: "installed by
// <version>" when the marker records a strictly older plumb than the running
// binary, "unknown version / hand-edited" when there is no marker, and plain
// "stale" when the marker matches or exceeds the running version or cannot
// be parsed (see versionOlder) — a marker at the running version means the
// content drifted after installation, which the version cannot explain.
//
// A skill's reference notes count towards its state: a current SKILL.md whose
// references/ note is missing or drifted reports stale, not installed. The
// alternative — grading on SKILL.md alone — would tell a user everything is
// current while the very file SKILL.md sends them to is absent, which is the
// failure the references embed exists to fix.
func skillStateAt(dir, name, content string, refs []embeddedFile) string {
	data, err := os.ReadFile(filepath.Join(dir, name, "SKILL.md"))
	switch {
	case err != nil:
		return skillStateMissing
	case stripSkillMarker(string(data)) == content && referencesCurrent(dir, name, refs):
		return skillStateInstalled
	default:
		marker, ok := skillMarkerVersion(string(data))
		switch {
		case !ok:
			return skillStateStale + " (unknown version / hand-edited)"
		case versionOlder(marker, Version):
			return fmt.Sprintf("%s (installed by %s)", skillStateStale, marker)
		default:
			return skillStateStale
		}
	}
}

// referencesCurrent reports whether every embedded reference note for skill
// name is present at <dir>/<name>/references/ with identical content. Reference
// notes carry no provenance marker, so the comparison is exact.
func referencesCurrent(dir, name string, refs []embeddedFile) bool {
	for _, ref := range refs {
		data, err := os.ReadFile(filepath.Join(dir, name, "references", ref.Name))
		if err != nil || string(data) != ref.Content {
			return false
		}
	}
	return true
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
		if skillStateAt(dir, s.Name, s.Content, s.References) != skillStateInstalled {
			return dir, true
		}
	}
	return dir, false
}

// skillDriftCounts tallies the non-current skills in dir, for the doctor
// grade's message (skillsDrift only answers whether there is anything to
// say). The stale states carry provenance suffixes, hence the prefix match.
func skillDriftCounts(dir string) (missing, stale int) {
	for _, s := range embeddedSkills() {
		switch state := skillStateAt(dir, s.Name, s.Content, s.References); {
		case state == skillStateMissing:
			missing++
		case strings.HasPrefix(state, skillStateStale):
			stale++
		}
	}
	return missing, stale
}

// skillStaleDetails lists one entry per stale skill in dir — the skill name
// plus its provenance suffix, e.g. "plumb-git (installed by 0.15.1)" — so the
// doctor detail carries the same source-version information as the status
// table. A plain "stale" state (marker at/above the running version) yields
// the bare name.
func skillStaleDetails(dir string) []string {
	var out []string
	for _, s := range embeddedSkills() {
		state := skillStateAt(dir, s.Name, s.Content, s.References)
		if strings.HasPrefix(state, skillStateStale) {
			out = append(out, s.Name+strings.TrimPrefix(state, skillStateStale))
		}
	}
	return out
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
