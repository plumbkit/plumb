package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

func projectConfigPool(t *testing.T) *workspacePool {
	t.Helper()
	cfg := config.Defaults()
	goCfg := cfg.LSP["go"]
	goCfg.Command = os.Args[0]
	goCfg.Enabled = true
	goCfg.Diagnostics = "push"
	cfg.LSP = map[string]config.LSPConfig{"go": goCfg}
	return newWorkspacePool(context.Background(), cfg)
}

func writePoolProjectConfig(t *testing.T, workspace, body string) {
	t.Helper()
	dir := filepath.Join(workspace, ".plumb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspacePoolConfigForWorkspaceAppliesProjectDiagnostics(t *testing.T) {
	pool := projectConfigPool(t)
	pullWorkspace := t.TempDir()
	defaultWorkspace := t.TempDir()
	writePoolProjectConfig(t, pullWorkspace, "[lsp.go]\ndiagnostics = \"pull\"\n")

	pullCfg, ok := pool.cfgForWorkspace(pullWorkspace, "go")
	if !ok {
		t.Fatal("project-configured Go adapter was not resolved")
	}
	if pullCfg.Diagnostics != "pull" {
		t.Fatalf("project diagnostics = %q, want pull", pullCfg.Diagnostics)
	}

	defaultCfg, ok := pool.cfgForWorkspace(defaultWorkspace, "go")
	if !ok {
		t.Fatal("default Go adapter was not resolved")
	}
	if defaultCfg.Diagnostics != "push" {
		t.Fatalf("sibling workspace diagnostics = %q, want unchanged global push", defaultCfg.Diagnostics)
	}
}

func TestWorkspacePoolConfigForWorkspaceHonoursDisable(t *testing.T) {
	pool := projectConfigPool(t)
	workspace := t.TempDir()
	writePoolProjectConfig(t, workspace, "[lsp.go]\nenabled = false\n")

	if _, ok := pool.cfgForWorkspace(workspace, "go"); ok {
		t.Fatal("a project-disabled language must not start a pooled server")
	}
}

func TestWorkspacePoolConfigForWorkspaceInvalidProjectFallsBackGlobal(t *testing.T) {
	pool := projectConfigPool(t)
	workspace := t.TempDir()
	writePoolProjectConfig(t, workspace, "[lsp.go\nnot valid toml")

	got, ok := pool.cfgForWorkspace(workspace, "go")
	if !ok {
		t.Fatal("invalid project config should fall back to the valid global adapter")
	}
	if got.Diagnostics != "push" {
		t.Fatalf("fallback diagnostics = %q, want global push", got.Diagnostics)
	}
}
