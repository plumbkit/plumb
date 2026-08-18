package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// The uninstall tests pin the safety properties the feature promises: plumb's
// entry goes, siblings survive, everything is backed up first, and a repeat
// (or a never-registered client) is a quiet no-op.

func writeTestConfig(t *testing.T, path string, cfg map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating config dir: %v", err)
	}
	if err := writeJSON(path, cfg); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
}

func assertHasBackup(t *testing.T, pathWithoutExt string) {
	t.Helper()
	pattern := pathWithoutExt + ".*.bak*"
	if matches, err := filepath.Glob(pattern); err != nil || len(matches) == 0 {
		t.Errorf("expected a backup matching %s, got %v (err %v)", pattern, matches, err)
	}
}

func TestRemoveServerEntry_JSONRoundTrip(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, map[string]any{
		"otherTop": "kept",
		"mcpServers": map[string]any{
			"other": map[string]any{"command": "/bin/other"},
			"plumb": map[string]any{"command": "/bin/plumb", "args": []any{"serve"}},
		},
	})

	removed, err := removeMcpServersJSON(cfgPath)
	if err != nil || !removed {
		t.Fatalf("removeServerEntry = (%v, %v), want (true, nil)", removed, err)
	}

	cfg, err := parseJSONConfig(cfgPath)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["plumb"]; ok {
		t.Error("plumb entry still present after removal")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server was removed with plumb")
	}
	if cfg["otherTop"] != "kept" {
		t.Error("unrelated top-level key was dropped")
	}
	assertHasBackup(t, cfgPath)

	removed, err = removeMcpServersJSON(cfgPath)
	if err != nil || removed {
		t.Errorf("repeat removal = (%v, %v), want (false, nil)", removed, err)
	}
}

func TestRemoveServerEntry_EmptyServersKeyDropped(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, map[string]any{
		"mcpServers": map[string]any{
			"plumb": map[string]any{"command": "/bin/plumb"},
		},
	})

	if removed, err := removeMcpServersJSON(cfgPath); err != nil || !removed {
		t.Fatalf("removeServerEntry = (%v, %v), want (true, nil)", removed, err)
	}
	cfg, err := parseJSONConfig(cfgPath)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	if _, ok := cfg["mcpServers"]; ok {
		t.Error("an mcpServers left empty should be dropped, restoring the pre-plumb shape")
	}
}

func TestRemoveServerEntry_AbsentFileCreatesNothing(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "missing.json")
	removed, err := removeMcpServersJSON(cfgPath)
	if err != nil || removed {
		t.Fatalf("absent config = (%v, %v), want (false, nil)", removed, err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("uninstall must not create a config file")
	}
}

func TestRemoveServerEntry_NonObjectServersIsNoop(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	before := `{"mcpServers": [1, 2]}` + "\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeMcpServersJSON(cfgPath)
	if err != nil || removed {
		t.Fatalf("non-object servers key = (%v, %v), want (false, nil)", removed, err)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(after) != before {
		t.Error("a config plumb cannot have registered into must be left byte-identical")
	}
}

func TestRemoveServerEntry_CodexTOML(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := map[string]any{
		"model": "x",
		"mcp_servers": map[string]any{
			"other": map[string]any{"command": "/bin/other"},
			"plumb": map[string]any{"command": "/bin/plumb", "args": []any{"serve"}},
		},
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if removed, err := removeCodexTOML(cfgPath); err != nil || !removed {
		t.Fatalf("removeCodexTOML = (%v, %v), want (true, nil)", removed, err)
	}
	parsed, err := parseTOMLConfig(cfgPath)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	servers := parsed["mcp_servers"].(map[string]any)
	if _, ok := servers["plumb"]; ok {
		t.Error("plumb table still present after removal")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server table was removed with plumb")
	}
	assertHasBackup(t, cfgPath)
}

func TestSetupZCodeOut(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, map[string]any{
		"hooks": map[string]any{"onStart": true},
		"mcp": map[string]any{"servers": map[string]any{
			"plumb": map[string]any{"type": "stdio", "command": "/bin/plumb", "args": []any{"serve"}},
			"other": map[string]any{"command": "/bin/other"},
		}},
	})

	if removed, err := setupZCodeOut(cfgPath); err != nil || !removed {
		t.Fatalf("setupZCodeOut = (%v, %v), want (true, nil)", removed, err)
	}
	cfg, err := parseJSONConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mcp := cfg["mcp"].(map[string]any)
	servers := mcp["servers"].(map[string]any)
	if _, ok := servers["plumb"]; ok {
		t.Error("nested mcp.servers.plumb still present")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server was removed")
	}
	if _, ok := cfg["hooks"]; !ok {
		t.Error("ZCode's own hooks section must survive")
	}

	// An mcp.servers holding only plumb drops the servers key but keeps "mcp".
	writeTestConfig(t, cfgPath, map[string]any{
		"mcp": map[string]any{"servers": map[string]any{
			"plumb": map[string]any{"command": "/bin/plumb"},
		}},
	})
	if removed, err := setupZCodeOut(cfgPath); err != nil || !removed {
		t.Fatalf("setupZCodeOut (only plumb) = (%v, %v), want (true, nil)", removed, err)
	}
	cfg, _ = parseJSONConfig(cfgPath)
	mcp = cfg["mcp"].(map[string]any)
	if _, ok := mcp["servers"]; ok {
		t.Error("an mcp.servers left empty should be dropped")
	}
}

func TestSetupDSHOut(t *testing.T) {
	t.Run("fresh registration round-trips to an empty layer", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "cordis.patch.yml")
		if _, _, err := setupDSHInto(cfgPath, "/bin/plumb"); err != nil {
			t.Fatal(err)
		}
		if removed, err := setupDSHOut(cfgPath); err != nil || !removed {
			t.Fatalf("setupDSHOut = (%v, %v), want (true, nil)", removed, err)
		}
		if _, registered, err := dshCommandExtractor(cfgPath); err != nil || registered {
			t.Errorf("extractor after removal = (%v, %v), want registered=false", registered, err)
		}
	})

	t.Run("sibling rows in the insert entry survive", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "cordis.patch.yml")
		src := `- id: keep
  insert:
    - id: mcp-plumb
      name: "@deepseek-ai/dsh-mcp-client"
      config:
        serverName: plumb
        transport: stdio
        command: /bin/plumb
        args: [serve]
    - id: other-plugin
      name: other
`
		if err := os.WriteFile(cfgPath, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if removed, err := setupDSHOut(cfgPath); err != nil || !removed {
			t.Fatalf("setupDSHOut = (%v, %v), want (true, nil)", removed, err)
		}
		data, _ := os.ReadFile(cfgPath)
		got := string(data)
		if _, registered, _ := dshCommandExtractor(cfgPath); registered {
			t.Error("plumb row still registered after removal")
		}
		for _, want := range []string{"other-plugin", "keep"} {
			if !strings.Contains(got, want) {
				t.Errorf("patch lost %q after removal:\n%s", want, got)
			}
		}
		assertHasBackup(t, cfgPath)
	})

	t.Run("absent layer creates nothing", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "absent.yml")
		if removed, err := setupDSHOut(cfgPath); err != nil || removed {
			t.Fatalf("setupDSHOut on absent file = (%v, %v), want (false, nil)", removed, err)
		}
		if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
			t.Error("uninstall must not create a patch file")
		}
	})
}

func TestSetupAntigravityOut(t *testing.T) {
	base := t.TempDir()
	sharedFlat := filepath.Join(base, "config", "mcp_config.json")
	surfaceFlat := filepath.Join(base, "antigravity-cli", "mcp_config.json")
	standalone := filepath.Join(base, "antigravity", "mcp", "plumb.json")
	ideMirror := filepath.Join(base, "antigravity-ide", "mcp", "plumb.json")

	for _, p := range []string{sharedFlat, surfaceFlat} {
		writeTestConfig(t, p, map[string]any{
			"mcpServers": map[string]any{
				"plumb": map[string]any{"command": "/bin/plumb", "args": []any{"serve"}},
				"other": map[string]any{"command": "/bin/other"},
			},
		})
	}
	writeTestConfig(t, standalone, map[string]any{"command": "/bin/plumb", "args": []any{"serve"}})
	writeTestConfig(t, ideMirror, map[string]any{"command": "/bin/plumb", "args": []any{"serve"}})

	if removed, err := setupAntigravityOut(standalone); err != nil || !removed {
		t.Fatalf("setupAntigravityOut = (%v, %v), want (true, nil)", removed, err)
	}

	cfg, err := parseJSONConfig(sharedFlat)
	if err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["plumb"]; ok {
		t.Error("plumb still in the shared flat config")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server lost from the shared flat config")
	}
	if _, err := os.Stat(surfaceFlat); err != nil {
		t.Fatalf("surface flat config vanished: %v", err)
	}
	if cfg, err = parseJSONConfig(surfaceFlat); err != nil {
		t.Fatal(err)
	}
	servers = cfg["mcpServers"].(map[string]any)
	if _, ok := servers["plumb"]; ok {
		t.Error("plumb still in the surface flat config")
	}
	for _, gone := range []string{standalone, ideMirror} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted, stat err = %v", gone, err)
		}
	}
	assertHasBackup(t, standalone[:len(standalone)-len(".json")])
}

func TestRemovePlumbSkills(t *testing.T) {
	skills := embeddedSkills()
	if len(skills) < 2 {
		t.Fatalf("test needs at least two embedded skills, have %d", len(skills))
	}
	dir := t.TempDir()

	ours, rewritten := skills[0], skills[1]
	oursDir := filepath.Join(dir, ours.Name)
	if err := os.MkdirAll(filepath.Join(oursDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oursDir, "SKILL.md"), []byte(stampSkillContent(ours.Content)), 0o644); err != nil {
		t.Fatal(err)
	}

	rewrittenDir := filepath.Join(dir, rewritten.Name)
	if err := os.MkdirAll(rewrittenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No provenance marker and different content: a skill the user made their own.
	if err := os.WriteFile(filepath.Join(rewrittenDir, "SKILL.md"), []byte("# my own\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, kept := removePlumbSkills(dir)
	if len(removed) != 1 || removed[0] != ours.Name {
		t.Errorf("removed = %v, want [%s]", removed, ours.Name)
	}
	if len(kept) != 1 || kept[0] != rewritten.Name {
		t.Errorf("kept = %v, want [%s]", kept, rewritten.Name)
	}
	if _, err := os.Stat(oursDir); !os.IsNotExist(err) {
		t.Error("plumb-owned skill dir should be removed")
	}
	if _, err := os.Stat(rewrittenDir); err != nil {
		t.Error("user-rewritten skill dir must survive")
	}
	// The backup directory sits beside the removed skill, not inside it.
	matches, _ := filepath.Glob(filepath.Join(dir, ours.Name+".*.bak"))
	if len(matches) == 0 {
		t.Error("skill backup directory missing")
	} else if _, err := os.Stat(filepath.Join(matches[0], "SKILL.md")); err != nil {
		t.Errorf("backup exists but holds no SKILL.md: %v", err)
	}
}
