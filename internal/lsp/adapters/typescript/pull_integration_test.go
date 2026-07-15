//go:build integration

package typescript_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	ts "github.com/plumbkit/plumb/internal/lsp/adapters/typescript"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// spawnTSRaw starts typescript-language-server and returns an un-initialised
// adapter, so the caller can drive Initialize with custom (pull) capabilities.
func spawnTSRaw(t *testing.T) *ts.Adapter {
	t.Helper()
	bin := requireTSServer(t)

	cmd := exec.Command(bin, "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("stdin pipe:", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("stdout pipe:", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatal("start typescript-language-server:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })
	return ts.New(conn)
}

// TestIntegration_ForcedPull_MethodNotFound is the TypeScript half of the
// validation matrix. Even when plumb forces the pull client capability on
// (ClientCapabilitiesFor(true)), typescript-language-server 5.3 advertises NO
// diagnosticProvider and answers textDocument/diagnostic with -32601. That is
// exactly the "pull-requested-but-unavailable" downgrade signal: the resolved
// mode would flip back to push. The push path itself is unaffected — proven by
// TestIntegration_DidChangeWatchedFiles, which stays green.
func TestIntegration_ForcedPull_MethodNotFound(t *testing.T) {
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "typescript-fixture")
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"tsconfig.json", filepath.Join("src", "greeter.ts")} {
		src, err := os.ReadFile(filepath.Join(fixtureSrc, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, rel), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ad := spawnTSRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Force the pull client capability on, exactly as the pool does under
	// [lsp.typescript] diagnostics = "pull".
	params := ts.DefaultInitParams(protocol.FileURI(ws))
	params.Capabilities = protocol.ClientCapabilitiesFor(true)
	params.InitializationOptions = tsServerInitOptions(t)
	res, err := ad.Initialize(ctx, params)
	if err != nil {
		skipIfNoTSInstall(t, err)
		t.Fatal("initialize (pull caps):", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	// (1) Despite advertising the pull client capability, the server does NOT
	// advertise diagnosticProvider.
	if ad.SupportsPullDiagnostics() {
		t.Errorf("typescript-language-server %s unexpectedly advertised diagnosticProvider "+
			"under forced pull caps — the -32601 downgrade evidence no longer holds", tsVersion())
	}
	_, hasOpts := res.Capabilities.DiagnosticOptions()
	t.Logf("OBSERVED typescript-language-server: diagnosticProvider advertised=%v "+
		"(SupportsPullDiagnostics=%v)", hasOpts, ad.SupportsPullDiagnostics())

	// (2) A document pull returns -32601 (method not found) — the downgrade
	// signal the pool acts on.
	greeterURI := protocol.FileURI(filepath.Join(ws, "src", "greeter.ts"))
	src, _ := os.ReadFile(filepath.Join(ws, "src", "greeter.ts"))
	_ = ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: greeterURI, LanguageID: "typescript", Version: 1, Text: string(src),
		},
	})
	_, pullErr := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: greeterURI},
	})
	if pullErr == nil {
		t.Fatal("expected textDocument/diagnostic to fail on typescript-language-server, got nil error")
	}
	if !jsonrpc.IsMethodNotFound(pullErr) {
		t.Fatalf("expected -32601 method-not-found from textDocument/diagnostic, got: %v", pullErr)
	}
	t.Logf("OBSERVED typescript-language-server document pull error (expected -32601): %v", pullErr)
}

// tsVersion best-effort reports the installed server version for log context.
func tsVersion() string {
	bin, err := exec.LookPath("typescript-language-server")
	if err != nil {
		return "?"
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "?"
	}
	return string(out[:len(out)-1])
}
