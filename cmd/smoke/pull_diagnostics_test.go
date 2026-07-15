//go:build integration

package smoke_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSmoke_PullDiagnosticsModes exercises the public configuration contract
// over a real MCP connection and gopls process. The default remains quiet push;
// an explicit [lsp.go] opt-in negotiates hybrid mode and serves a clean result
// from an actual textDocument/diagnostic request.
func TestSmoke_PullDiagnosticsModes(t *testing.T) {
	requireGopls(t)
	plumbBin := buildPlumb(t)

	t.Run("default stays push", func(t *testing.T) {
		fixture := makeFixture(t)
		tmpHome := mkTmpHome(t)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		client := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
		client.initialize(t, fixture)
		sessionOut := client.call(t, "session_start",
			map[string]any{"workspace": fixture}, sessionStartTimeout)

		assertContains(t, "default session_start", sessionOut, "LSP is ready")
		if strings.Contains(sessionOut, "(diagnostics:") {
			t.Fatalf("default auto mode must stay quiet push; session_start reported a non-default mode:\n%s", sessionOut)
		}
	})

	t.Run("explicit pull negotiates hybrid", func(t *testing.T) {
		fixture := makeFixture(t)
		config := []byte("[lsp.go]\ndiagnostics = \"pull\"\n")
		if err := os.WriteFile(filepath.Join(fixture, ".plumb", "config.toml"), config, 0o644); err != nil {
			t.Fatal("write fixture config:", err)
		}

		tmpHome := mkTmpHome(t)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		client := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
		client.initialize(t, fixture)
		sessionOut := client.call(t, "session_start",
			map[string]any{"workspace": fixture}, sessionStartTimeout)
		assertContains(t, "pull session_start", sessionOut, "LSP is ready (diagnostics: pull)")

		mainGo := filepath.Join(fixture, "main.go")
		diagnosticsOut := client.call(t, "diagnostics",
			map[string]any{"uri": mainGo}, toolTimeout)
		assertContains(t, "pull diagnostics", diagnosticsOut, "pulled from the language server")
		assertContains(t, "pull diagnostics", diagnosticsOut, "file is clean")

		// A real write exercises the pull-mode post-write path. gopls also
		// pushes for the created file, so the pool must classify the connection
		// as hybrid without losing the immediate pulled finding.
		brokenGo := filepath.Join(fixture, "broken.go")
		writeOut := client.call(t, "write_file", map[string]any{
			"file_path":         brokenGo,
			"content":           "package main\n\nfunc broken() int { return \"oops\" }\n",
			"await_diagnostics": true,
		}, toolTimeout)
		assertContains(t, "pull post-write diagnostics", writeOut, "new since this edit")
		if strings.Contains(writeOut, "UNVERIFIED") {
			t.Fatalf("successful gopls post-write pull was reported unverified:\n%s", writeOut)
		}

		assertEventuallyContains(t, 10*time.Second, "hybrid daemon_info", func() string {
			return client.call(t, "daemon_info", map[string]any{}, toolTimeout)
		}, "diagnostics: hybrid")
		client.call(t, "delete_file", map[string]any{"file_path": brokenGo}, toolTimeout)
	})
}
