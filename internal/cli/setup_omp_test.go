package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOmpAgentDir_FollowsOmpPrecedence pins where plumb writes for omp. Each
// variable relocates omp's agent directory, and a resolver that ignored one
// would register plumb in a file omp never reads while reporting success.
func TestOmpAgentDir_FollowsOmpPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		unsetOMP   bool
		wantRelHom []string // joined under HOME; nil when wantAbs is used
		wantAbs    string
	}{
		{name: "default", env: map[string]string{}, unsetOMP: true, wantRelHom: []string{".omp", "agent"}},
		{name: "PI_CODING_AGENT_DIR replaces the default dir", env: map[string]string{"PI_CODING_AGENT_DIR": "/custom/agent"}, unsetOMP: true, wantAbs: "/custom/agent"},
		{name: "PI_CONFIG_DIR renames the root", env: map[string]string{"PI_CONFIG_DIR": ".omp-alt"}, unsetOMP: true, wantRelHom: []string{".omp-alt", "agent"}},
		{name: "named profile", env: map[string]string{"OMP_PROFILE": "work"}, wantRelHom: []string{".omp", "profiles", "work", "agent"}},
		{name: "named profile ignores PI_CODING_AGENT_DIR", env: map[string]string{"OMP_PROFILE": "work", "PI_CODING_AGENT_DIR": "/custom/agent"}, wantRelHom: []string{".omp", "profiles", "work", "agent"}},
		{name: "default profile name is the default profile", env: map[string]string{"OMP_PROFILE": " default ", "PI_CODING_AGENT_DIR": "/custom/agent"}, wantAbs: "/custom/agent"},
		{name: "PI_PROFILE when OMP_PROFILE is undefined", env: map[string]string{"PI_PROFILE": "legacy"}, unsetOMP: true, wantRelHom: []string{".omp", "profiles", "legacy", "agent"}},
		{name: "empty OMP_PROFILE outranks PI_PROFILE", env: map[string]string{"OMP_PROFILE": "", "PI_PROFILE": "legacy"}, wantRelHom: []string{".omp", "agent"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			for _, k := range []string{"OMP_PROFILE", "PI_PROFILE", "PI_CODING_AGENT_DIR", "PI_CONFIG_DIR"} {
				t.Setenv(k, tc.env[k])
			}
			if tc.unsetOMP {
				// t.Setenv above registered the restore; Unsetenv makes it undefined.
				if err := os.Unsetenv("OMP_PROFILE"); err != nil {
					t.Fatal(err)
				}
			}
			want := tc.wantAbs
			if tc.wantRelHom != nil {
				want = filepath.Join(append([]string{home}, tc.wantRelHom...)...)
			}
			got, err := ompAgentDir()
			if err != nil {
				t.Fatalf("ompAgentDir: %v", err)
			}
			if got != want {
				t.Errorf("agent dir: got %q, want %q", got, want)
			}
		})
	}
}

// TestOmpRepoint_KeepsUserFields covers the omp-specific keys a repoint must
// not drop: the file's $schema line and a per-server timeout, which plumb users
// raise because check_messages and git calls can outlast omp's 30 s default.
func TestOmpRepoint_KeepsUserFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	const existing = `{
	"$schema": "https://example.invalid/mcp-schema.json",
	"mcpServers": {
		"plumb": {"type": "stdio", "command": "/old/plumb", "args": ["serve"], "timeout": 1200000},
		"other": {"command": "other-bin"}
	}
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	added, preserved, err := setupClaudeCodeInto(path, "/new/plumb")
	if err != nil || !added {
		t.Fatalf("repoint: added=%v err=%v", added, err)
	}
	if len(preserved) != 1 || preserved[0] != "other" {
		t.Errorf("preserved: got %v, want [other]", preserved)
	}
	cfg, err := parseJSONConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["$schema"] != "https://example.invalid/mcp-schema.json" {
		t.Errorf("$schema lost: %v", cfg["$schema"])
	}
	entry := cfg["mcpServers"].(map[string]any)["plumb"].(map[string]any)
	if entry["command"] != "/new/plumb" || entry["type"] != "stdio" {
		t.Errorf("entry not repointed: %v", entry)
	}
	if entry["timeout"] != float64(1200000) {
		t.Errorf("timeout lost on repoint: %v", entry["timeout"])
	}
}
