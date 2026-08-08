package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillFreshnessResult covers every state the doctor pass can see. The
// drift case must stay informational (ok, no warn, no fix): stale skills are
// not a broken integration, they are content one `plumb skills sync` away, so
// a "!" would inflate doctor's warning count for a repair. Unregistered and
// no-channel clients produce no line at all — the grade must never suggest
// syncing a client whose config does not use plumb.
func TestSkillFreshnessResult(t *testing.T) {
	register := func(t *testing.T, dir string) (setupTarget, string) {
		t.Helper()
		cfg := filepath.Join(dir, "mcp.json")
		skillsDir := filepath.Join(dir, "skills")
		if _, _, err := setupClaudeDesktopInto(cfg, "/opt/plumb"); err != nil {
			t.Fatal(err)
		}
		return skillsTestTarget(cfg, skillsDir), skillsDir
	}

	t.Run("unregistered client produces no line", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "mcp.json")
		if err := os.WriteFile(cfg, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := skillFreshnessResult(skillsTestTarget(cfg, filepath.Join(dir, "skills"))); ok {
			t.Error("a client whose config does not register plumb must produce no line")
		}
	})

	t.Run("absent config produces no line", func(t *testing.T) {
		dir := t.TempDir()
		if _, ok := skillFreshnessResult(skillsTestTarget(filepath.Join(dir, "mcp.json"), t.TempDir())); ok {
			t.Error("a client with no config file must produce no line")
		}
	})

	t.Run("missing skills are informational", func(t *testing.T) {
		target, _ := register(t, t.TempDir())
		res, ok := skillFreshnessResult(target)
		if !ok {
			t.Fatal("a registered client with no skills installed must produce a line")
		}
		if !res.ok || res.warn || res.fix != "" {
			t.Errorf("skill drift must be informational (clean pass, no fix): %+v", res)
		}
		if !strings.Contains(res.detail, "plumb skills sync test-client") {
			t.Errorf("detail must name the fix: %q", res.detail)
		}
	})

	t.Run("current skills produce no line", func(t *testing.T) {
		target, _ := register(t, t.TempDir())
		if _, results := installSkillsFor(target); len(results) == 0 {
			t.Fatal("expected the skills to install")
		}
		if _, ok := skillFreshnessResult(target); ok {
			t.Error("a client with current skills must produce no line")
		}
	})

	t.Run("one stale skill is reported", func(t *testing.T) {
		target, skillsDir := register(t, t.TempDir())
		if _, results := installSkillsFor(target); len(results) == 0 {
			t.Fatal("expected the skills to install")
		}
		stale := filepath.Join(skillsDir, embeddedSkills()[0].Name, "SKILL.md")
		if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res, ok := skillFreshnessResult(target)
		if !ok {
			t.Fatal("a stale skill must produce a line")
		}
		if !strings.Contains(res.detail, "1 stale") {
			t.Errorf("detail should count the stale skill: %q", res.detail)
		}
		if !strings.Contains(res.detail, "unknown version / hand-edited") {
			t.Errorf("a markerless stale skill must report its provenance as unknown: %q", res.detail)
		}
	})

	t.Run("stale skill reports its installing version", func(t *testing.T) {
		pinVersion(t, "0.16.3")
		target, skillsDir := register(t, t.TempDir())
		if _, results := installSkillsFor(target); len(results) == 0 {
			t.Fatal("expected the skills to install")
		}
		stale := filepath.Join(skillsDir, embeddedSkills()[0].Name, "SKILL.md")
		if err := os.WriteFile(stale, []byte("<!-- plumb: 0.15.1 -->\nstale\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res, ok := skillFreshnessResult(target)
		if !ok {
			t.Fatal("a stale skill must produce a line")
		}
		if !strings.Contains(res.detail, "installed by 0.15.1") {
			t.Errorf("detail must name the installing version: %q", res.detail)
		}
	})

	t.Run("no skill channel produces no line", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "mcp.json")
		if _, _, err := setupClaudeDesktopInto(cfg, "/opt/plumb"); err != nil {
			t.Fatal(err)
		}
		target := setupTarget{
			use: "plain", name: "Plain Client",
			pathFn:    func() (string, error) { return cfg, nil },
			extractFn: claudeDesktopCommandExtractor,
		}
		if _, ok := skillFreshnessResult(target); ok {
			t.Error("a client with no skill channel must produce no line")
		}
	})
}
