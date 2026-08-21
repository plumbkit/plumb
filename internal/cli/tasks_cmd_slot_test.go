package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeJSWorkspace(t *testing.T, extra string) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"package.json":       `{"name":"demo"}`,
		"pnpm-lock.yaml":     "",
		".plumb/config.toml": extra,
	} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func runTaskCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := taskSlotCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String(), err
}

// The five fixed verbs are registered at package init from the built-in list, so
// a project-defined slot can never have one. This is the verb that reaches it.
func TestTaskSlotCmd_IsRegisteredAlongsideTheBuiltinVerbs(t *testing.T) {
	names := make([]string, 0, len(taskCmds))
	for _, c := range taskCmds {
		names = append(names, strings.Fields(c.Use)[0])
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"build", "lint", "test", "e2e", "verify", "task"} {
		if !strings.Contains(joined, want) {
			t.Errorf("command %q is not registered; got %v", want, names)
		}
	}
}

func TestTaskSlotCmd_RejectsMalformedSlotName(t *testing.T) {
	for _, bad := range []string{"Bad Name", "9build", "check!"} {
		if _, err := runTaskCmd(t, bad); err == nil {
			t.Errorf("slot %q must be refused", bad)
		} else if !strings.Contains(err.Error(), "not a valid slot name") {
			t.Errorf("slot %q: unexpected error %v", bad, err)
		}
	}
}

// A bare `plumb task` teaches the workspace's vocabulary — including the
// project's own slots — rather than printing usage.
func TestTaskSlotCmd_BareListsConfiguredSlotsIncludingExtras(t *testing.T) {
	ws := writeJSWorkspace(t, "[tasks.typescript]\ncheck = \"echo checked\"\n")
	t.Chdir(ws)

	out, err := runTaskCmd(t)
	if err != nil {
		t.Fatalf("plumb task: %v (out=%s)", err, out)
	}
	for _, want := range []string{"check", "echo checked", "build", "test"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing should mention %q, got:\n%s", want, out)
		}
	}
	// verify is a composite; printing an empty command would read as unconfigured.
	if strings.Contains(out, "verify      \n") {
		t.Errorf("verify must not render as an empty command, got:\n%s", out)
	}
}

// The typescript defaults must follow the lockfile through the CLI path too,
// not just through the MCP resolver.
func TestTaskSlotCmd_ListingShowsLockfileDerivedDefaults(t *testing.T) {
	ws := writeJSWorkspace(t, "")
	t.Chdir(ws)

	out, err := runTaskCmd(t)
	if err != nil {
		t.Fatalf("plumb task: %v (out=%s)", err, out)
	}
	if !strings.Contains(out, "pnpm run build") {
		t.Errorf("a pnpm-lock.yaml workspace should show pnpm defaults, got:\n%s", out)
	}
	if strings.Contains(out, "npm run build") && !strings.Contains(out, "pnpm run build") {
		t.Errorf("npm default leaked through on a pnpm workspace:\n%s", out)
	}
}
