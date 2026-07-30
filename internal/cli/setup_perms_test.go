package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSetupWriters_PreserveThirdPartyConfigMode is the regression for a real
// defect: `plumb setup` edits config files belonging to OTHER tools (~/.codex,
// the Claude config, a Gemini tree). Those writers staged through
// os.CreateTemp — which always creates 0600 — and never chmod'd, so the first
// time plumb merged its MCP entry into a user's existing 0644 config it
// silently tightened the file's permissions as a side effect.
//
// Rewriting an existing file must never change its mode.
func TestSetupWriters_PreserveThirdPartyConfigMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}

	cases := []struct {
		name  string
		file  string
		write func(path string) error
	}{
		{"json", "config.json", func(p string) error { return writeJSON(p, map[string]any{"k": "v"}) }},
		{"toml", "config.toml", func(p string) error { return writeTOML(p, map[string]any{"k": "v"}) }},
		{"yaml", "config.yaml", func(p string) error { return writeYAML(p, map[string]any{"k": "v"}) }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, c.file)

			// A config file as a third-party tool would have left it.
			if err := os.WriteFile(path, []byte("existing: true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := c.write(path); err != nil {
				t.Fatalf("write: %v", err)
			}

			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != 0o644 {
				t.Errorf("mode = %04o after rewrite, want 0644 — plumb setup must not change a third-party file's permissions", got)
			}

			// And nothing staged is left in the foreign directory.
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("directory contains %v, want only %s", names, c.file)
			}
		})
	}
}

// TestInstallSkill_PreservesExistingMode covers the same hazard for the skill
// installer, which writes markdown into another tool's skills directory.
func TestInstallSkill_PreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	skillsDir := t.TempDir()
	dst := filepath.Join(skillsDir, "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := installSkill(skillsDir, "demo", "new content\n")
	if err != nil {
		t.Fatalf("installSkill: %v", err)
	}
	if status != "updated" {
		t.Errorf("status = %q, want %q", status, "updated")
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %04o after update, want 0644", got)
	}
	body, _ := os.ReadFile(dst)
	if string(body) != "new content\n" {
		t.Errorf("content = %q, want the new content", body)
	}
}
