package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/plumbkit/plumb/internal/fsync"
)

// This file is `plumb skills sync`'s hash ledger: the manifest that lets a
// sync tell "plumb's own content changed between versions" (replace in
// place, no backup) from "the user edited this file themselves" (never
// overwrite it) — and the cleanup pass that removes the directory-level
// ".bak" litter a naive overwrite-and-backup policy used to leave behind
// (see backupSkillDir, still used by `plumb setup --uninstall`, which is
// where that litter has been coming from).

// skillManifestEntry records the content hash and plumb version last written
// for one skill.
type skillManifestEntry struct {
	Hash    string `json:"hash"`
	Version string `json:"version"`
}

// skillManifest is skill name -> the entry installSkill last wrote for it.
// One manifest lives per skills directory (per client), since each client's
// directory is synced and drifts independently.
type skillManifest struct {
	Skills map[string]skillManifestEntry `json:"skills"`
}

// skillManifestPath returns the manifest file for skillsDir, kept under a
// ".plumb" subdirectory so it is never mistaken for a skill bundle by a
// client that scans skillsDir's entries for a "<name>/SKILL.md" shape: a
// dot-prefixed directory containing no SKILL.md at all matches neither the
// name nor the shape a skill loader looks for.
func skillManifestPath(skillsDir string) string {
	return filepath.Join(skillsDir, ".plumb", "skills-manifest.json")
}

// loadSkillManifest reads skillsDir's manifest, returning an empty one
// (never nil) when the file does not exist yet — the case for every client
// synced before this manifest existed, and the very first sync of a fresh
// client.
func loadSkillManifest(skillsDir string) (*skillManifest, error) {
	data, err := os.ReadFile(skillManifestPath(skillsDir))
	if os.IsNotExist(err) {
		return &skillManifest{Skills: map[string]skillManifestEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading skills manifest: %w", err)
	}
	var m skillManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing skills manifest %s: %w", skillManifestPath(skillsDir), err)
	}
	if m.Skills == nil {
		m.Skills = map[string]skillManifestEntry{}
	}
	return &m, nil
}

// saveSkillManifest writes m to skillsDir's manifest atomically, creating the
// ".plumb" directory if needed.
func saveSkillManifest(skillsDir string, m *skillManifest) error {
	dir := filepath.Join(skillsDir, ".plumb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating manifest directory: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding skills manifest: %w", err)
	}
	data = append(data, '\n')
	return fsync.AtomicWrite(skillManifestPath(skillsDir), data, setupWriteOptions(".plumb_skills_manifest_*.json"))
}

// cloneSkillManifest returns a value-copy of m's entries, used to snapshot
// "what the manifest said before this sync ran" so the cleanup pass below
// can compare a backup's hash against both the old and the newly-written
// shipped hash.
func cloneSkillManifest(m *skillManifest) *skillManifest {
	out := &skillManifest{Skills: make(map[string]skillManifestEntry, len(m.Skills))}
	for k, v := range m.Skills {
		out.Skills[k] = v
	}
	return out
}

// hashSkillContent returns the hex sha256 of content — the unit the manifest
// and the cleanup pass compare, always computed over marker-stripped text so
// a version bump alone never looks like a content change.
func hashSkillContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// seedBootstrapHashes fills before with the same pre-manifest fallback
// installSkill uses (lastShippedHash's marker-based branch) for every
// embedded skill not already recorded — so the FIRST sync a directory ever
// runs under this manifest still knows "what was on disk a moment ago" for
// the cleanup pass below, rather than treating everything as unknown just
// because the manifest file itself did not exist yet. Without this, a
// skill whose content legitimately changed between plumb versions would
// have its pre-sync backup correctly identified as a real update in
// installSkill, but the SAME backup would be reported as "needs manual
// review" by cleanupSkillBackups purely because it predates the manifest —
// two functions disagreeing about the same fact from the same run.
func seedBootstrapHashes(skillsDir string, before *skillManifest) {
	for _, skill := range embeddedSkills() {
		if _, ok := before.Skills[skill.Name]; ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(skillsDir, skill.Name, "SKILL.md"))
		if err != nil {
			continue
		}
		h, ok := lastShippedHash(before, skill.Name, string(data))
		if !ok {
			continue
		}
		version, _ := skillMarkerVersion(string(data))
		if version == "" {
			version = Version
		}
		before.Skills[skill.Name] = skillManifestEntry{Hash: h, Version: version}
	}
}

// lastShippedHash returns the hash installSkill should treat as "what plumb
// shipped last time" for name, and whether one is known at all. The manifest
// is authoritative when it already has an entry. Absent that — a client
// synced before this manifest existed, or a directory nothing has ever
// written to under this mechanism — a file that already carries plumb's own
// provenance marker is trusted as plumb's, using its stripped content as the
// implicit prior hash. Without this fallback, turning the manifest on would
// read every already-installed skill as "user modified" on its first sync.
func lastShippedHash(m *skillManifest, name, existingRaw string) (hash string, known bool) {
	if e, ok := m.Skills[name]; ok {
		return e.Hash, true
	}
	if _, ok := skillMarkerVersion(existingRaw); ok {
		return hashSkillContent(stripSkillMarker(existingRaw)), true
	}
	return "", false
}

// skillBackupDirPattern matches the directory-level backups backupSkillDir
// creates: "<name>.<YYYYMMDD-HHMMSS>.bak", sibling to the skill directories
// themselves — the shape that showed up duplicated in a live skills
// directory (see PLAN-365).
var skillBackupDirPattern = regexp.MustCompile(`^(.+)\.\d{8}-\d{6}\.bak$`)

// skillCleanupReport is one sync's outcome for the ".bak" cleanup pass.
type skillCleanupReport struct {
	removed []string
	kept    []string
	err     error
}

// cleanupSkillBackups deletes the directory-level ".bak" backups found in
// skillsDir, but ONLY when their content is provably plumb's own: the
// backup's SKILL.md, marker stripped, hashes to either the shipped hash
// recorded for that skill name before this sync ran (before) or the one it
// just wrote (after). A backup whose name does not match a currently
// embedded skill, or whose content matches neither hash, is left in place
// and reported in kept for manual review — never deleted on a guess (a
// backup can carry the user's own pre-plumb content, from before the
// directory belonged to plumb at all).
func cleanupSkillBackups(skillsDir string, before, after *skillManifest, dryRun bool) skillCleanupReport {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return skillCleanupReport{}
		}
		return skillCleanupReport{err: fmt.Errorf("reading %s: %w", skillsDir, err)}
	}

	var report skillCleanupReport
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := skillBackupDirPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if !backupMatchesShippedHash(skillsDir, e.Name(), m[1], before, after) {
			report.kept = append(report.kept, e.Name())
			continue
		}
		if dryRun {
			report.removed = append(report.removed, e.Name())
			continue
		}
		if err := os.RemoveAll(filepath.Join(skillsDir, e.Name())); err != nil {
			report.kept = append(report.kept, e.Name())
			continue
		}
		report.removed = append(report.removed, e.Name())
	}
	sort.Strings(report.removed)
	sort.Strings(report.kept)
	return report
}

// backupMatchesShippedHash reports whether backupName's SKILL.md hashes to a
// shipped hash plumb has on record for skillName, in either the pre-sync or
// post-sync manifest.
func backupMatchesShippedHash(skillsDir, backupName, skillName string, before, after *skillManifest) bool {
	data, err := os.ReadFile(filepath.Join(skillsDir, backupName, "SKILL.md"))
	if err != nil {
		return false
	}
	h := hashSkillContent(stripSkillMarker(string(data)))
	if e, ok := before.Skills[skillName]; ok && e.Hash == h {
		return true
	}
	if e, ok := after.Skills[skillName]; ok && e.Hash == h {
		return true
	}
	return false
}
