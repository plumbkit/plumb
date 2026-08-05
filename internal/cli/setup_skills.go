package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/fsync"
	"github.com/plumbkit/plumb/internal/render"
)

// This file is the whole skill-delivery seam: which clients consume plumb's
// embedded SKILL.md files, where each one keeps them, and the install/refresh
// that both the named `plumb setup <client>` commands and the bulk
// --all/--install-missing sweep drive.
//
// WHICH CLIENTS. Skill capability is a per-client fact carried as
// setupTarget.skillsDirFn, and it is verified per client rather than inferred.
// The claim "client X reads SKILL.md from directory Y" rots fast and is exactly
// the kind of thing a model will assert confidently and wrongly, so each
// resolver below records what was checked and against which version. A client
// with no verified skills directory gets nil, and its steering arrives as the
// condensed session_start guidance block instead — not a directory plumb
// guessed at.

// setupNoSkillFlag backs --no-skill. It is shared by every skill-capable
// client's command and by the bulk sweep, so opting out is one flag rather than
// one per client.
var setupNoSkillFlag bool

// registerNoSkillFlag adds --no-skill to a command whose target installs skills.
// It is driven off skillsDirFn rather than hand-registered per command, so a
// client that gains a skills directory gains the opt-out with it.
func registerNoSkillFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&setupNoSkillFlag, "no-skill", false,
		"Skip installing plumb's skill files")
}

// claudeSkillsDir returns the user-scoped Claude Code skills directory
// (~/.claude/skills). It does not create the directory.
//
// Verified: this is the directory Claude Code has read since plumb first shipped
// skills, and the shape the other two resolvers below were checked against.
func claudeSkillsDir() (string, error) {
	return homeRelConfigPath(".claude", "skills")
}

// codexSkillsDir returns the user-scoped Codex skills directory, honouring the
// same CODEX_HOME precedence as CodexConfigPath.
//
// Verified against a live install (codex-cli 0.145.0), not assumed: its own
// skill-authoring instructions say to "default to $CODEX_HOME/skills; when
// CODEX_HOME is unset, fall back to ~/.codex/skills so the skill is
// auto-discovered", and it loads the <name>/SKILL.md directory-bundle shape
// plumb already writes. Codex was one of the clients that received no steering
// at all after the routing prose left the tool descriptions; it turns out to
// have had a skill channel the whole time.
func codexSkillsDir() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "skills"), nil
	}
	return homeRelConfigPath(".codex", "skills")
}

// kimiCodeSkillsDir returns the user-scoped Kimi Code skills directory,
// honouring the same KIMI_CODE_HOME precedence as KimiCodeConfigPath.
//
// Verified against a live install (Kimi Code 0.32.0), not assumed: it resolves
// user-scoped skills under "$KIMI_CODE_HOME/skills/, or ~/.kimi-code/skills/ if
// the env var is not set", reads the <name>/SKILL.md directory-bundle shape, and
// requires the frontmatter name + description that every shipped plumb skill
// already carries (TestEmbeddedSkills_HaveValidFrontmatter pins that).
func kimiCodeSkillsDir() (string, error) {
	if home := os.Getenv("KIMI_CODE_HOME"); home != "" {
		return filepath.Join(home, "skills"), nil
	}
	return homeRelConfigPath(".kimi-code", "skills")
}

// installAndPrintSkills installs the embedded skills into t's skills directory
// and prints one line per skill that changed. It is a no-op for a target with no
// skills directory and under --no-skill.
//
// Errors are non-fatal by design: a failed skill install must not fail a
// registration that otherwise succeeded, because the MCP entry is the part the
// client cannot work without.
func installAndPrintSkills(t setupTarget) {
	dir, results := installSkillsFor(t)
	for _, r := range results {
		switch {
		case r.err != nil:
			fmt.Fprintf(os.Stderr, "warning: installing skill %q: %v\n", r.name, r.err)
		case r.action != "unchanged":
			fmt.Printf("Skill %-20s %s → %s\n", r.name, r.action, filepath.Join(dir, r.name, "SKILL.md"))
		}
	}
}

// skillResult is one skill's outcome: the action installSkill reported
// ("installed"/"updated"/"unchanged"), or the error that stopped it.
type skillResult struct {
	name   string
	action string
	err    error
}

// installSkillsFor is the shared, non-printing body: it installs every embedded
// skill into t's skills directory and reports what happened to each. Both the
// named-command path (installAndPrintSkills) and the bulk sweep (refreshSkills)
// drive it, so the two can never diverge on which skills are installed or where.
//
// A target with no skills directory, or a --no-skill run, yields no results at
// all — distinct from "results that all say unchanged", which is what a
// no-op refresh of a skill-capable client looks like.
func installSkillsFor(t setupTarget) (dir string, results []skillResult) {
	if t.skillsDirFn == nil || setupNoSkillFlag {
		return "", nil
	}
	dir, err := t.skillsDirFn()
	if err != nil {
		return "", []skillResult{{name: t.name, err: fmt.Errorf("resolving skills directory: %w", err)}}
	}
	for _, skill := range embeddedSkills() {
		action, err := installSkill(dir, skill.Name, skill.Content)
		results = append(results, skillResult{name: skill.Name, action: action, err: err})
	}
	return dir, results
}

// plumbIsRegistered reports whether a refreshClientAt status means the client
// now carries a plumb entry. It gates the skill refresh: "not installed", "not
// registered", and "error" all mean plumb is not in that config, and writing
// skills for a client that does not use plumb would populate a directory the
// user never pointed at plumb.
func plumbIsRegistered(status string) bool {
	switch status {
	case "already current", "updated", "registered":
		return true
	default:
		return false
	}
}

// refreshSkills installs or refreshes one client's skills during the bulk sweep
// and reports a table row's worth of outcome. An empty status means there was
// nothing to report — the client has no skill channel, or --no-skill was passed.
func refreshSkills(c setupTarget) (status, detail string, changed bool) {
	dir, results := installSkillsFor(c)
	if len(results) == 0 {
		return "", "", false
	}
	var failed error
	for _, r := range results {
		switch {
		case r.err != nil:
			failed = r.err
		case r.action != "unchanged":
			changed = true
		}
	}
	if failed != nil {
		return "skills error", failed.Error(), changed
	}
	if changed {
		return "skills updated", render.ContractPath(dir), true
	}
	return "skills current", render.ContractPath(dir), false
}

// installSkill writes content to <skillsDir>/<name>/SKILL.md, creating
// the directory if needed. Returns "installed", "updated", or "unchanged".
// If the file already exists with different content it is backed up first.
// Atomic write via temp-file + rename.
func installSkill(skillsDir, name, content string) (string, error) {
	dir := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating skill directory: %w", err)
	}

	dst := filepath.Join(dir, "SKILL.md")
	existing, readErr := os.ReadFile(dst)

	switch {
	case readErr == nil && string(existing) == content:
		return "unchanged", nil
	case readErr == nil:
		// File exists but content differs — back up before overwriting.
		if err := backupFile(dst); err != nil {
			return "", fmt.Errorf("backing up %s: %w", dst, err)
		}
	case os.IsNotExist(readErr):
		// File does not exist — fresh install, no backup needed.
	default:
		return "", fmt.Errorf("reading %s: %w", dst, readErr)
	}

	if err := fsync.AtomicWrite(dst, []byte(content), setupWriteOptions(".plumb_skill_*.md")); err != nil {
		return "", fmt.Errorf("installing skill: %w", err)
	}

	if os.IsNotExist(readErr) {
		return "installed", nil
	}
	return "updated", nil
}
