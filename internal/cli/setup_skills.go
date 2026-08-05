package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/fsync"
)

// This file is the whole skill-delivery seam: which clients consume plumb's
// embedded SKILL.md files, where each one keeps them, and the install that
// `plumb skills sync` drives. Registration (`plumb setup`) is deliberately
// config-only: it never writes skill files, it only points at `plumb skills
// sync` when it detects drift (see printSkillsDriftHint in skills_cmd.go).
//
// WHICH CLIENTS. Skill capability is a per-client fact carried as
// setupTarget.skillsDirFn, and it is verified per client rather than inferred.
// The claim "client X reads SKILL.md from directory Y" rots fast and is exactly
// the kind of thing a model will assert confidently and wrongly, so each
// resolver below records what was checked and against which version. A client
// with no verified skills directory gets nil, and its steering arrives as the
// condensed session_start guidance block instead — not a directory plumb
// guessed at.

// skillCapableClients returns the setup targets that declare a skills
// directory — the set `plumb skills` reports on and `plumb skills sync` sweeps.
func skillCapableClients() []setupTarget {
	var out []setupTarget
	for _, c := range allSetupClients() {
		if c.skillsDirFn != nil {
			out = append(out, c)
		}
	}
	return out
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

// skillResult is one skill's outcome: the action installSkill reported
// ("installed"/"updated"/"unchanged"), or the error that stopped it.
type skillResult struct {
	name   string
	action string
	err    error
}

// installSkillsFor is the shared, non-printing body: it installs every embedded
// skill into t's skills directory and reports what happened to each. `plumb
// skills sync` drives it for every registered skill-capable client (or one named
// client), so the sweep and the named form can never diverge on which skills
// are installed or where.
//
// A target with no skills directory yields no results at all — distinct from
// "results that all say unchanged", which is what a no-op refresh of a
// skill-capable client looks like.
func installSkillsFor(t setupTarget) (dir string, results []skillResult) {
	if t.skillsDirFn == nil {
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
