package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/setup"
	"github.com/plumbkit/plumb/internal/tui"
)

// setupGlobalInstructionsFlag additionally writes plumb's managed
// instruction block to the client's GLOBAL instruction file
// (~/.codex/AGENTS.md, ~/.claude/CLAUDE.md, ~/.gemini/GEMINI.md). Off by
// default: unlike a project file, a global file is shared across every
// project a client touches and may hold content the user wrote — a bare
// `plumb setup <client>` never writes it, only this explicit opt-in does.
var setupGlobalInstructionsFlag bool

// registerGlobalInstructionsFlag wires --global onto a client command whose
// target declares instructionsFn.
func registerGlobalInstructionsFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&setupGlobalInstructionsFlag, "global", false,
		"Also write plumb's managed instruction block to the client's global instructions file")
}

// codexInstructionPaths returns Codex's project-level AGENTS.md — Codex
// reads AGENTS.md directly, with no CLAUDE.md-style symlink convention — and
// its global ~/.codex/AGENTS.md.
func codexInstructionPaths() (project, global string, err error) {
	cwd, home, err := instructionRoots()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(cwd, "AGENTS.md"), filepath.Join(home, ".codex", "AGENTS.md"), nil
}

// geminiInstructionPaths returns Gemini CLI's project-level GEMINI.md and its
// global ~/.gemini/GEMINI.md — the same config directory `plumb setup gemini`
// already writes ~/.gemini/settings.json into.
func geminiInstructionPaths() (project, global string, err error) {
	cwd, home, err := instructionRoots()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(cwd, "GEMINI.md"), filepath.Join(home, ".gemini", "GEMINI.md"), nil
}

// claudeCodeInstructionPaths returns Claude Code's project-level CLAUDE.md
// (which, per this very repo's layout, may be a symlink to AGENTS.md —
// setup.Apply follows it and rewrites the target, never the link) and its
// global ~/.claude/CLAUDE.md.
func claudeCodeInstructionPaths() (project, global string, err error) {
	cwd, home, err := instructionRoots()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(cwd, "CLAUDE.md"), filepath.Join(home, ".claude", "CLAUDE.md"), nil
}

func instructionRoots() (cwd, home string, err error) {
	cwd, err = os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("getting working directory: %w", err)
	}
	home, err = os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("locating home directory: %w", err)
	}
	return cwd, home, nil
}

// applyInstructionsBlock writes plumb's managed instruction block for t —
// always to the project-level file, and to the global-level file too when
// --global was passed. A target with no instructionsFn (most of them, for
// now — only claude-code/codex/gemini carry one) is silently a no-op, per
// the "no auto-writes without an explicit setup/init invocation" rule: a
// target that never gets an instructionsFn never gets a write.
//
// Which BODY gets written differs between the two files. The project file
// goes through templateForProject, which checks whether t's project path is
// shared with another instruction-capable client (a symlinked CLAUDE.md/
// GEMINI.md -> AGENTS.md layout, this repo's own) and, if so, writes the
// shared/client-agnostic DefaultTemplate instead of t's own — see
// templateForGroup for why this is what keeps three clients' templates from
// fighting over one span. The global file is never shared this way (
// ~/.codex, ~/.claude, ~/.gemini always name distinct directories), so it
// always gets t's own template when one exists.
func applyInstructionsBlock(t setupTarget) ([]string, error) {
	if t.instructionsFn == nil {
		return nil, nil
	}
	project, global, err := t.instructionsFn()
	if err != nil {
		return nil, fmt.Errorf("locating %s instructions file: %w", t.name, err)
	}

	projectBody, err := templateForProject(t, project)
	if err != nil {
		return nil, fmt.Errorf("resolving instructions template for %s: %w", t.name, err)
	}

	changed, err := setup.Apply(project, projectBody, setup.DefaultVersion)
	if err != nil {
		return nil, fmt.Errorf("writing managed instruction block to %s: %w", project, err)
	}
	lines := []string{instructionsResultLine(project, changed)}

	if setupGlobalInstructionsFlag {
		globalBody := clientTemplateOrDefault(t.use)
		changed, err := setup.Apply(global, globalBody, setup.DefaultVersion)
		if err != nil {
			return lines, fmt.Errorf("writing managed instruction block to %s: %w", global, err)
		}
		lines = append(lines, instructionsResultLine(global, changed))
	}
	return lines, nil
}

// clientTemplateOrDefault returns client's own template (setup.
// ClientTemplates, keyed by setupTarget.use) when one is registered, or
// setup.DefaultTemplate otherwise.
func clientTemplateOrDefault(client string) string {
	if body, ok := setup.TemplateForClient(client); ok {
		return body
	}
	return setup.DefaultTemplate
}

// templateForProject resolves the body that should be written to t's
// project-level instruction file: t's own template when t is the SOLE
// instruction-capable client naming that real file, or the shared
// DefaultTemplate when the file's canonical path is also named by another
// client (found via groupInstructionFiles, the same grouping --check/--sync
// use). Basing the choice on the file's grouping — a fact of the project's
// symlink topology, not of which command happened to run — is what makes
// repeated `plumb setup <client>` calls in any order converge on the same
// content instead of oscillating between different clients' templates.
func templateForProject(t setupTarget, project string) (string, error) {
	groups, err := groupInstructionFiles()
	if err != nil {
		return "", err
	}
	key := paths.Canonical(project)
	for _, g := range groups {
		if paths.Canonical(g.project) == key {
			return templateForGroup(g), nil
		}
	}
	// t wasn't found among instructionCapableClients' groups — should not
	// happen given t.instructionsFn != nil, but fall back to t's own
	// template (or DefaultTemplate) rather than erroring.
	return clientTemplateOrDefault(t.use), nil
}

func instructionsResultLine(path string, changed bool) string {
	if changed {
		return "Instructions: " + path + " (managed block written)"
	}
	return "Instructions: " + path + " (already current)"
}

// removeInstructionsBlock deletes plumb's managed instruction block from t's
// project-level instruction file — and the global one too when --global was
// passed, mirroring applyInstructionsBlock's own project/global split. A
// target with no instructionsFn is silently a no-op, and so is a file that
// never had a block (setup.Remove itself is a no-op in both cases).
//
// On a shared/symlinked project file (CLAUDE.md/GEMINI.md -> AGENTS.md), the
// block is client-agnostic (templateForGroup) and this removes it outright —
// uninstalling one client from a file it shares with others still-registered
// removes the whole file's block, since v1 has no per-client scoping within
// one shared span. That is a deliberate, documented trade-off, not an
// oversight: see the PR body.
func removeInstructionsBlock(t setupTarget) ([]string, error) {
	if t.instructionsFn == nil {
		return nil, nil
	}
	project, global, err := t.instructionsFn()
	if err != nil {
		return nil, fmt.Errorf("locating %s instructions file: %w", t.name, err)
	}

	var lines []string
	removed, err := setup.Remove(project)
	if err != nil {
		return nil, fmt.Errorf("removing managed instruction block from %s: %w", project, err)
	}
	if removed {
		lines = append(lines, "Instructions: "+project+" (managed block removed)")
	}

	if setupGlobalInstructionsFlag {
		removed, err := setup.Remove(global)
		if err != nil {
			return lines, fmt.Errorf("removing managed instruction block from %s: %w", global, err)
		}
		if removed {
			lines = append(lines, "Instructions: "+global+" (managed block removed)")
		}
	}
	return lines, nil
}

// printInstructionsResult writes t's managed instruction block(s) and prints
// the result. Errors are reported, not fatal — a setup command that already
// registered the MCP server successfully should not fail outright over the
// instructions file (a read-only project directory, say).
func printInstructionsResult(t setupTarget) {
	lines, err := applyInstructionsBlock(t)
	if err != nil {
		fmt.Printf("\nWarning: %v\n", err)
		return
	}
	for _, line := range lines {
		fmt.Println(line)
	}
}

// setupCheckFlag and setupSyncFlag are the top-level `plumb setup --check` /
// `--sync` modes: report (or fix) managed-instruction-block drift for this
// project's client instruction files, without touching MCP registration.
var (
	setupCheckFlag bool
	setupSyncFlag  bool
)

// instructionCapableClients lists the setup targets that declare an
// instructionsFn — the ones --check/--sync inspect. Order is display order.
func instructionCapableClients() []setupTarget {
	var clients []setupTarget
	for _, t := range allSetupClients() {
		if t.instructionsFn != nil {
			clients = append(clients, t)
		}
	}
	return clients
}

// instructionFileGroup is every client whose project-level instruction path
// resolves — via paths.Canonical — to the SAME real file. On this repo's own
// layout, for instance, CLAUDE.md and GEMINI.md both symlink to AGENTS.md, so
// claude-code/codex/gemini all name one file. Grouping is what keeps
// --check/--sync from Check-ing or Apply-ing that one file three times over
// (previously: three rows for one file), and — via templateForGroup — from
// three clients' differing per-client templates fighting over which one last
// wrote it. uses parallels names but carries each client's setupTarget.use
// key (e.g. "codex"), which is what ClientTemplates is keyed by; names holds
// the display form ("Codex") shown in the --check/--sync table.
type instructionFileGroup struct {
	names   []string
	uses    []string
	project string
}

// templateForGroup returns the managed-block body that should be written to
// a group's real file: the sole client's own template when it is the only
// client naming this file, or the shared, client-agnostic DefaultTemplate
// when more than one client's project path resolves to the same real file.
// Because the choice depends only on the file's (stable) symlink topology —
// never on which client or command happened to run, or in what order — every
// caller that reaches the same group computes the same body, so repeated
// `plumb setup <client>` / `--sync` calls converge on one piece of content
// instead of oscillating between different clients' templates.
func templateForGroup(g instructionFileGroup) string {
	if len(g.uses) == 1 {
		return clientTemplateOrDefault(g.uses[0])
	}
	return setup.DefaultTemplate
}

// groupInstructionFiles resolves every instruction-capable client's
// project-level path and groups the ones that name the same real file,
// preserving first-seen order.
func groupInstructionFiles() ([]instructionFileGroup, error) {
	index := map[string]int{} // canonical path -> position in groups
	var groups []instructionFileGroup
	for _, target := range instructionCapableClients() {
		project, _, err := target.instructionsFn()
		if err != nil {
			return nil, fmt.Errorf("locating %s instructions file: %w", target.name, err)
		}
		key := paths.Canonical(project)
		if i, ok := index[key]; ok {
			groups[i].names = append(groups[i].names, target.name)
			groups[i].uses = append(groups[i].uses, target.use)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, instructionFileGroup{names: []string{target.name}, uses: []string{target.use}, project: project})
	}
	return groups, nil
}

// runSetupInstructionsCheckOrSync implements `plumb setup --check` and
// `plumb setup --sync`. Both inspect the same set of project-level
// instruction files — deduplicated by real file, see groupInstructionFiles —
// and --sync additionally rewrites any that drifted. Global files are out of
// scope here — --check/--sync tracks the project a user is standing in,
// matching Check/Apply everywhere else in this file rather than silently
// touching a shared global file the user never asked about here.
//
// A malformed file (setup.StatusMalformed, or an Apply refusal during
// --sync) is reported IN ITS ROW rather than aborting the whole run: the
// point of listing every client's file in one pass is to see all of them,
// and one file needing hand repair should not hide the status of the rest.
func runSetupInstructionsCheckOrSync(sync bool) error {
	PrintLogo()
	t := render.NewGroupedTable(tui.SepStyle, tui.HintStyle, "Client", "File", "Status")

	groups, err := groupInstructionFiles()
	if err != nil {
		return err
	}

	drift := false
	for _, g := range groups {
		label := strings.Join(g.names, " / ")
		body := templateForGroup(g)

		if sync {
			changed, err := setup.Apply(g.project, body, setup.DefaultVersion)
			if err != nil {
				drift = true
				t.Row(label, render.ShortenPath(g.project, setupPathWidth), "error: "+err.Error())
				continue
			}
			status := "current"
			if changed {
				status = "synced"
				drift = true
			}
			t.Row(label, render.ShortenPath(g.project, setupPathWidth), status)
			continue
		}

		status, err := setup.Check(g.project, body, setup.DefaultVersion)
		if err != nil {
			return fmt.Errorf("checking %s: %w", label, err)
		}
		if status != setup.StatusCurrent {
			drift = true
		}
		t.Row(label, render.ShortenPath(g.project, setupPathWidth), status.String())
	}

	fmt.Println(t.Render())
	if !sync && drift {
		fmt.Println("\nRun `plumb setup --sync` to rewrite drifted instruction files to the current template version.")
	}
	return nil
}
