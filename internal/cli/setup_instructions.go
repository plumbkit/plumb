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
func applyInstructionsBlock(t setupTarget) ([]string, error) {
	if t.instructionsFn == nil {
		return nil, nil
	}
	project, global, err := t.instructionsFn()
	if err != nil {
		return nil, fmt.Errorf("locating %s instructions file: %w", t.name, err)
	}

	changed, err := setup.Apply(project, setup.DefaultTemplate, setup.DefaultVersion)
	if err != nil {
		return nil, fmt.Errorf("writing managed instruction block to %s: %w", project, err)
	}
	lines := []string{instructionsResultLine(project, changed)}

	if setupGlobalInstructionsFlag {
		changed, err := setup.Apply(global, setup.DefaultTemplate, setup.DefaultVersion)
		if err != nil {
			return lines, fmt.Errorf("writing managed instruction block to %s: %w", global, err)
		}
		lines = append(lines, instructionsResultLine(global, changed))
	}
	return lines, nil
}

func instructionsResultLine(path string, changed bool) string {
	if changed {
		return "Instructions: " + path + " (managed block written)"
	}
	return "Instructions: " + path + " (already current)"
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
// (previously: three rows for one file) — and, looking ahead to per-client
// templates, from three clients fighting over which template last wrote it.
type instructionFileGroup struct {
	names   []string
	project string
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
			continue
		}
		index[key] = len(groups)
		groups = append(groups, instructionFileGroup{names: []string{target.name}, project: project})
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

		if sync {
			changed, err := setup.Apply(g.project, setup.DefaultTemplate, setup.DefaultVersion)
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

		status, err := setup.Check(g.project, setup.DefaultTemplate, setup.DefaultVersion)
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
