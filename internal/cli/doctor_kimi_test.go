package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKimiToolSelectResultAt(t *testing.T) {
	cases := []struct {
		name   string
		config string // "" means no config file at all
		warn   bool
	}{
		{"no config", "", false},
		{"no experimental table", "default_model = \"k\"\n", false},
		{"flag off", "[experimental]\ntool-select = false\n", false},
		{"other flag on", "[experimental]\nsomething-else = true\n", false},
		{"unparsable", "[experimental\ntool-select = true\n", false},
		{"flag on", "default_model = \"k\"\n\n[experimental]\ntool-select = true\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if tc.config != "" {
				if err := os.WriteFile(path, []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r, ok := kimiToolSelectResultAt(path)
			if ok != tc.warn {
				t.Fatalf("row = %v, want %v (%+v)", ok, tc.warn, r)
			}
			if !ok {
				return
			}
			if !r.ok || !r.warn {
				t.Errorf("tool-select must be a warning, never a failure: ok=%v warn=%v", r.ok, r.warn)
			}
			if !strings.Contains(r.detail, "issues/2381") || !strings.Contains(r.fix, path) {
				t.Errorf("row must cite the upstream issue and name the file to fix:\n%s\n%s", r.detail, r.fix)
			}
		})
	}
}

// TestCheckKimiToolSelect_FollowsKimiCodeHome pins the wiring: the check reads
// config.toml beside the mcp.json that setup writes, so KIMI_CODE_HOME moves
// both.
func TestCheckKimiToolSelect_FollowsKimiCodeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	if got := checkKimiToolSelect(); len(got) != 0 {
		t.Fatalf("no config yet, got %+v", got)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[experimental]\ntool-select = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checkKimiToolSelect(); len(got) != 1 || !got[0].warn {
		t.Fatalf("flag on under KIMI_CODE_HOME, got %+v", got)
	}
}
