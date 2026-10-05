package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sensitiveMarker = "diff withheld: sensitive path"

// agent_config reports the config.toml change it applied, through the same
// policy as the file tools: the diff when the path is ordinary, the
// withholding marker when [history] sensitive_globs names it.
func TestAgentConfigResponseShowsTheConfigDiff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		projGlobs string
		wantDiff  bool
	}{
		{"ordinary path shows the diff", "", true},
		{"a sensitive glob withholds it", "[history]\nsensitive_globs = [\"config.toml\"]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			store, ss := newOriginStore(t)
			root := freshTempDir(t)
			mustGitDir(t, root)
			if tc.projGlobs != "" {
				if err := os.MkdirAll(filepath.Join(root, ".plumb"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".plumb", "config.toml"), []byte(tc.projGlobs), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := newPersistSession(t, store, ss, "proxy-agent-config-diff")
			if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
				t.Fatalf("pin: %v", err)
			}
			s.mutate(func(v *sessionView) { v.agentConfigWrites = true })
			out, err := s.applyAgentConfig(context.Background(), map[string]any{"tasks.go.build": "go build ./cmd/..."})
			if err != nil {
				t.Fatalf("applyAgentConfig: %v", err)
			}
			// The summary line names only the key; the value appears only in a diff.
			shown := strings.Contains(out, "@@") && strings.Contains(out, "go build ./cmd/...")
			if shown != tc.wantDiff {
				t.Errorf("diff shown = %v, want %v:\n%s", shown, tc.wantDiff, out)
			}
			if withheld := strings.Contains(out, sensitiveMarker); withheld == tc.wantDiff {
				t.Errorf("withheld marker present = %v, want %v:\n%s", withheld, !tc.wantDiff, out)
			}
		})
	}
}

// write_memory and delete_memory get their response diff (and its sensitivity
// gate) from the WriteDeps the daemon registers them with. Probed through the
// REAL registration, so dropping .WithWriteDeps from conn_register.go fails here.
func TestRegisteredMemoryWritersShowTheirDiff(t *testing.T) {
	s, srv := buildTestConnSession(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	run := func(name string, args map[string]any) string {
		t.Helper()
		tool, ok := srv.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		raw, _ := json.Marshal(args)
		out, err := tool.Execute(context.Background(), raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	const body = "wiring-probe-line"
	if out := run("write_memory", map[string]any{"name": "probe", "content": body + "\n"}); !strings.Contains(out, "+"+body) {
		t.Errorf("write_memory showed no diff of the body it wrote:\n%s", out)
	}
	if out := run("delete_memory", map[string]any{"name": "probe"}); !strings.Contains(out, "-"+body) {
		t.Errorf("delete_memory showed no diff of the lines it removed:\n%s", out)
	}
}
