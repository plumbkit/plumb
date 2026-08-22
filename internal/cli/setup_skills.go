package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// zcodeSkillsDir returns the user-scoped ZCode skills directory (~/.zcode/skills).
//
// Verified against the installed desktop app's own shipped documentation, not
// assumed: ZCode bundles a configuration guide inside the app that states user
// skills are discovered at ~/.zcode/skills/, loaded as <name>/SKILL.md directory
// bundles, and matched on frontmatter name + description — the exact shape every
// shipped plumb skill already carries (TestEmbeddedSkills_HaveValidFrontmatter
// pins that).
func zcodeSkillsDir() (string, error) {
	return homeRelConfigPath(".zcode", "skills")
}

// junieSkillsDir returns the user-scoped Junie skills directory (~/.junie/skills).
func junieSkillsDir() (string, error) {
	return homeRelConfigPath(".junie", "skills")
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
//
// dryRun computes and reports every action — including the cleanup pass —
// without writing SKILL.md, the manifest, a ".plumb-new" conflict file, or
// deleting a backup; it is `plumb skills sync --check`'s whole implementation.
// A skill whose SKILL.md is in conflict (see installSkill) has its reference
// notes left untouched too, rather than rewriting material next to a file the
// user is meant to review.
func installSkillsFor(t setupTarget, dryRun bool) (dir string, results []skillResult, cleanup skillCleanupReport) {
	if t.skillsDirFn == nil {
		return "", nil, skillCleanupReport{}
	}
	dir, err := t.skillsDirFn()
	if err != nil {
		return "", []skillResult{{name: t.name, err: fmt.Errorf("resolving skills directory: %w", err)}}, skillCleanupReport{}
	}

	manifest, err := loadSkillManifest(dir)
	if err != nil {
		return dir, []skillResult{{name: t.name, err: err}}, skillCleanupReport{}
	}
	before := cloneSkillManifest(manifest)

	for _, skill := range embeddedSkills() {
		action, err := installSkill(dir, skill.Name, skill.Content, manifest, dryRun)
		if err == nil && !strings.HasPrefix(action, skillActionConflict) {
			action, err = installSkillReferences(dir, skill, action, dryRun)
		}
		results = append(results, skillResult{name: skill.Name, action: action, err: err})
	}

	if !dryRun && !manifestsEqual(before, manifest) {
		if err := saveSkillManifest(dir, manifest); err != nil {
			results = append(results, skillResult{name: "manifest", err: fmt.Errorf("saving skills manifest: %w", err)})
		}
	}

	cleanup = cleanupSkillBackups(dir, before, manifest, dryRun)
	return dir, results, cleanup
}

// manifestsEqual reports whether a and b record the same entries — used to
// skip rewriting the manifest file when a sync changed nothing, so a no-op
// sync is a no-op on disk too, not just in its report.
func manifestsEqual(a, b *skillManifest) bool {
	if len(a.Skills) != len(b.Skills) {
		return false
	}
	for k, v := range a.Skills {
		if b.Skills[k] != v {
			return false
		}
	}
	return true
}

// installSkillReferences writes the skill's reference notes into
// <skillsDir>/<name>/references/, so a SKILL.md that points at one is pointing
// at a file the reader actually has.
//
// They are reported under the SKILL.md action rather than as rows of their own:
// a reference note is part of the skill, not a peer of it, and one row per file
// would make the sync table grow with material the user never asked about by
// name. The skill's action is promoted to the strongest of its parts, so a
// current SKILL.md beside a rewritten reference reports "updated" rather than
// the "unchanged" that would hide the write.
//
// Reference notes carry no provenance marker. The marker is an HTML comment,
// invisible in the markdown SKILL.md presentations render — but a reference is
// plain material a user may open in any viewer, and stamping it would buy
// nothing: staleness is already decided per skill by skillStateAt, which reads
// the references too.
//
// dryRun computes and reports the action without writing or backing up
// anything — `plumb skills sync --check`'s reference leg.
func installSkillReferences(skillsDir string, skill embeddedSkill, action string, dryRun bool) (string, error) {
	if len(skill.References) == 0 {
		return action, nil
	}
	dir := filepath.Join(skillsDir, skill.Name, "references")
	if !dryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return action, fmt.Errorf("creating references directory: %w", err)
		}
	}
	for _, ref := range skill.References {
		dst := filepath.Join(dir, ref.Name)
		existing, readErr := os.ReadFile(dst)
		switch {
		case readErr == nil && string(existing) == ref.Content:
			continue
		case readErr == nil:
			if !dryRun {
				if err := backupFile(dst); err != nil {
					return action, fmt.Errorf("backing up %s: %w", dst, err)
				}
			}
			action = strongerSkillAction(action, "updated")
		case os.IsNotExist(readErr):
			action = strongerSkillAction(action, "installed")
		default:
			return action, fmt.Errorf("reading %s: %w", dst, readErr)
		}
		if dryRun {
			continue
		}
		if err := fsync.AtomicWrite(dst, []byte(ref.Content), setupWriteOptions(".plumb_skill_ref_*.md")); err != nil {
			return action, fmt.Errorf("installing skill reference: %w", err)
		}
	}
	return action, nil
}

// strongerSkillAction merges two per-file outcomes into the one the skill row
// reports: "updated" outranks "installed", which outranks "unchanged". Ranking
// rather than last-write-wins is what stops a run that rewrote a reference from
// reporting "unchanged" because SKILL.md happened to be current.
func strongerSkillAction(a, b string) string {
	rank := map[string]int{"unchanged": 0, "installed": 1, "updated": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// skillActionConflict marks a skill whose on-disk SKILL.md cannot be proven
// to be plumb's own: either the manifest has no entry for it and content
// differs from what is being shipped now (a manifest-less directory can
// never prove ownership — see lastShippedHash), or the manifest's recorded
// hash does not match. Their file is left completely untouched; the
// proposed content is written instead to a "<name>.plumb-new" sibling FILE
// (not a directory, so it can never be mistaken for another skill bundle by
// a client that scans the skills directory for one) — UNLESS that file
// already holds this exact proposal, in which case nothing is rewritten, so
// re-running sync never clobbers a user's in-progress merge inside it. The
// action string carries which case applies: exactly skillActionConflict
// when the proposal was written or changed this run, or
// skillActionConflict+conflictUnchangedSuffix when it already matched.
const (
	skillActionConflict     = "conflict"
	conflictUnchangedSuffix = " (proposal unchanged)"
)

// installSkill writes content to <skillsDir>/<name>/SKILL.md, creating the
// directory if needed. Returns "installed", "updated", "unchanged", or
// skillActionConflict.
//
// manifest is the sync's hash ledger (see skillManifest): it is how
// installSkill tells "plumb's own content changed between versions" (a
// legitimate update — replaced in place, no backup) from "the file matches
// neither what plumb shipped last time nor what it is shipping now" (the
// user edited it — see skillActionConflict). manifest is updated in memory
// for the caller to persist; this function never writes it to disk.
//
// dryRun computes and reports the action without writing SKILL.md, the
// manifest, or a ".plumb-new" file — `plumb skills sync --check`'s per-skill
// leg.
func installSkill(skillsDir, name, content string, manifest *skillManifest, dryRun bool) (string, error) {
	dir := filepath.Join(skillsDir, name)
	dst := filepath.Join(dir, "SKILL.md")
	stamped := stampSkillContent(content)
	newHash := hashSkillContent(content)
	existing, readErr := os.ReadFile(dst)

	write := func(action string) (string, error) {
		manifest.Skills[name] = skillManifestEntry{Hash: newHash, Version: Version}
		if dryRun {
			return action, nil
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("creating skill directory: %w", err)
		}
		if err := fsync.AtomicWrite(dst, []byte(stamped), setupWriteOptions(".plumb_skill_*.md")); err != nil {
			return "", fmt.Errorf("installing skill: %w", err)
		}
		return action, nil
	}

	switch {
	case readErr == nil && string(existing) == stamped:
		manifest.Skills[name] = skillManifestEntry{Hash: newHash, Version: Version}
		return "unchanged", nil
	case readErr == nil && stripSkillMarker(string(existing)) == content:
		// Same skill with a stale or missing stamp — refresh the marker in
		// place. The content is untouched, so this is not an "update" and
		// carries no conflict risk.
		return write("unchanged")
	case readErr == nil:
		diskHash := hashSkillContent(stripSkillMarker(string(existing)))
		if oldHash, known := lastShippedHash(manifest, name); known && diskHash == oldHash {
			return write("updated")
		}
		return writeConflictProposal(skillsDir, name, stamped, dryRun)
	case os.IsNotExist(readErr):
		return write("installed")
	default:
		return "", fmt.Errorf("reading %s: %w", dst, readErr)
	}
}

// writeConflictProposal reports name as a conflict and, unless dryRun,
// writes stamped to "<name>.plumb-new" — but only when that differs from
// what is already there, so a re-run leaves the file alone when the
// proposal itself has not changed. This does NOT protect against a
// modified ".plumb-new": if the user has edited that file themselves (or
// left notes inside it), a differing proposal still replaces it outright —
// the guard only skips the write when content is already identical. See
// skillActionConflict for the two action strings this can return.
func writeConflictProposal(skillsDir, name, stamped string, dryRun bool) (string, error) {
	newFile := filepath.Join(skillsDir, name+".plumb-new")
	if existing, err := os.ReadFile(newFile); err == nil && string(existing) == stamped {
		return skillActionConflict + conflictUnchangedSuffix, nil
	}
	if dryRun {
		return skillActionConflict, nil
	}
	if err := fsync.AtomicWrite(newFile, []byte(stamped), setupWriteOptions(".plumb_skill_new_*.md")); err != nil {
		return "", fmt.Errorf("writing %s: %w", newFile, err)
	}
	return skillActionConflict, nil
}

// skillMarkerPrefix opens plumb's provenance marker — one HTML comment line,
// "<!-- plumb: <version> -->", recording which plumb build installed the
// skill. An HTML comment renders as nothing in markdown, so the line is
// invisible to every client's SKILL.md presentation.
const skillMarkerPrefix = "<!-- plumb: "

// skillLineEnding reports the line-ending sequence content appears to use:
// "\r\n" if its first newline is preceded by \r, otherwise "\n" (also the
// default for content with no newline at all, e.g. an empty file). A skill
// file is consistent throughout, so the first newline determines the whole
// file's convention. This codebase treats CRLF as first-class elsewhere
// (edit_file is documented CRLF-tolerant); stampSkillContent must match that
// norm rather than only ever matching a bare "\n" frontmatter delimiter —
// a CRLF skill file would otherwise fall through to the no-frontmatter
// branch and get its marker PREPENDED before the "---" line, corrupting the
// very frontmatter block this placement logic exists to protect.
func skillLineEnding(content string) string {
	if i := strings.IndexByte(content, '\n'); i > 0 && content[i-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// stampSkillContent returns content with the provenance marker recorded. When
// the skill opens with a YAML frontmatter block (--- lines) the marker goes
// immediately AFTER its closing delimiter — the verified consumers
// (claude-code, codex, kimi-code) parse frontmatter as the block between the
// leading --- lines, so anything inside it would corrupt the metadata, while
// anything after it is ordinary markdown. Without frontmatter the marker
// leads the file. The inserted marker (and the frontmatter delimiters it is
// matched against) use content's own line-ending style — an LF file is never
// rewritten to CRLF or vice versa.
func stampSkillContent(content string) string {
	nl := skillLineEnding(content)
	marker := skillMarkerPrefix + Version + " -->" + nl
	open := "---" + nl
	closeDelim := nl + "---" + nl
	if strings.HasPrefix(content, open) {
		if i := strings.Index(content[len(open):], closeDelim); i >= 0 {
			pos := len(open) + i + len(closeDelim)
			return content[:pos] + marker + content[pos:]
		}
	}
	return marker + content
}

// parseSkillMarker reports whether line is plumb's provenance marker and, if
// so, the version it records. line may carry a trailing "\r" (skillMarkerVersion
// and stripSkillMarker always split on bare "\n", so a CRLF file's lines keep
// their "\r"); it is trimmed before matching so both line-ending styles parse.
func parseSkillMarker(line string) (string, bool) {
	line = strings.TrimSuffix(line, "\r")
	if !strings.HasPrefix(line, skillMarkerPrefix) || !strings.HasSuffix(line, " -->") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(line, skillMarkerPrefix), " -->"), true
}

// skillMarkerVersion returns the version recorded by the first provenance
// marker in data, if one is present.
func skillMarkerVersion(data string) (string, bool) {
	for line := range strings.SplitSeq(data, "\n") {
		if v, ok := parseSkillMarker(line); ok {
			return v, true
		}
	}
	return "", false
}

// stripSkillMarker removes the first provenance marker line from data, so
// content comparisons see the skill itself rather than its stamp. A version
// bump alone must never read as drift. Splitting on bare "\n" leaves any
// "\r" attached to the surrounding lines, so removing the marker line and
// rejoining with "\n" reconstructs a CRLF file's original line endings
// without needing to special-case them here.
func stripSkillMarker(data string) string {
	lines := strings.Split(data, "\n")
	for i, line := range lines {
		if _, ok := parseSkillMarker(line); ok {
			return strings.Join(append(lines[:i], lines[i+1:]...), "\n")
		}
	}
	return data
}

// versionOlder reports whether a is an older release than b. The comparison
// is semver-ish: a leading "v" is stripped and numeric segments are compared
// in order, with missing segments read as zero. Any pre-release/build suffix
// (the "-rc.1" in "0.16.3-rc.1") is stripped before comparison rather than
// ordered against it, so a pre-release compares EQUAL to its release —
// "0.16.3-rc.1" is never "older" than "0.16.3". That is dormant today (the
// project reserves rc tags for v1.x, so no installed marker carries one yet)
// but is the actual behaviour, not merely "ignored" as ordering input. An
// unparseable side (a hand-typed marker, the "dev" build stamp) is never
// "older" either — the caller falls back to plain wording rather than
// inventing an ordering. Equal and newer are both false; only strictly older
// earns the "installed by" phrasing.
func versionOlder(a, b string) bool {
	pa, okA := parseVersionSegments(a)
	pb, okB := parseVersionSegments(b)
	if !okA || !okB {
		return false
	}
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// parseVersionSegments splits v into its numeric release segments.
func parseVersionSegments(v string) ([]int, bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		nums[i] = n
	}
	return nums, true
}
