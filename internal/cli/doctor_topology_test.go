package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProjectTopologyConfig drops a minimal project config that toggles the
// topology section, isolated under ws so config.LoadProject picks it up.
func writeProjectTopologyConfig(t *testing.T, ws string, enabled bool) {
	t.Helper()
	dir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .plumb: %v", err)
	}
	body := fmt.Sprintf("[topology]\nenabled = %v\n", enabled)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
}

func TestCheckTopology_DisabledByDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate from the developer's global config

	res := checkTopology("")
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d: %+v", len(res), res)
	}
	if !res[0].ok {
		t.Errorf("topology disabled should pass, got failure: %+v", res[0])
	}
	if !strings.Contains(res[0].detail, "disabled") {
		t.Errorf("detail = %q, want it to mention 'disabled'", res[0].detail)
	}
}

func TestCheckTopology_EnabledButNoIndex(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	writeProjectTopologyConfig(t, ws, true)

	res := checkTopology(ws)
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d: %+v", len(res), res)
	}
	if res[0].ok {
		t.Errorf("enabled with no index should fail, got pass: %+v", res[0])
	}
	if !strings.Contains(res[0].detail, "no index") {
		t.Errorf("detail = %q, want it to mention 'no index'", res[0].detail)
	}
	if res[0].fix == "" {
		t.Error("a failing topology check should carry a fix hint")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{3 * 1024 * 1024, "3.0 MiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
