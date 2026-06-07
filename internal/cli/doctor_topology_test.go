package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
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

func TestCheckTopology_DisabledByConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate from the developer's global config
	ws := t.TempDir()
	writeProjectTopologyConfig(t, ws, false) // topology is on by default; opt this workspace out

	res := checkTopology(ws)
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

func TestTopologyIndexHealth(t *testing.T) {
	cases := []struct {
		name      string
		st        topology.Status
		wantOK    bool
		wantWarn  bool
		detailSub string
	}{
		{
			name:      "cold start — nothing processed yet",
			st:        topology.Status{},
			wantOK:    true,
			wantWarn:  true,
			detailSub: "initial indexing",
		},
		{
			name:      "all files skipped",
			st:        topology.Status{SkippedFiles: 3},
			wantOK:    true,
			wantWarn:  true,
			detailSub: "no files indexed",
		},
		{
			name:      "indexed but no symbols",
			st:        topology.Status{IndexedFiles: 5},
			wantOK:    true,
			wantWarn:  true,
			detailSub: "no symbols extracted",
		},
		{
			name:      "healthy",
			st:        topology.Status{IndexedFiles: 5, TotalNodes: 42, TotalEdges: 10},
			wantOK:    true,
			wantWarn:  false,
			detailSub: "42 nodes",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := topologyIndexHealth(c.st)
			if res.ok != c.wantOK {
				t.Errorf("ok = %v, want %v", res.ok, c.wantOK)
			}
			if res.warn != c.wantWarn {
				t.Errorf("warn = %v, want %v", res.warn, c.wantWarn)
			}
			if !strings.Contains(res.detail, c.detailSub) {
				t.Errorf("detail = %q, want it to contain %q", res.detail, c.detailSub)
			}
			if c.wantWarn && res.fix == "" {
				t.Error("a warning should carry a fix hint")
			}
		})
	}
}
