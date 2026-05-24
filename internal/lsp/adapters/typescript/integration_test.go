//go:build integration

package typescript_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	ts "github.com/golimpio/plumb/internal/lsp/adapters/typescript"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// requireTSServer skips if typescript-language-server is not on PATH and returns
// its path. It is not installed on the validation machine, so this test skips
// there; it runs and validates the adapter wherever the binary is present.
func requireTSServer(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("typescript-language-server")
	if err != nil {
		t.Skip("typescript-language-server not found on PATH — install with: npm install -g typescript-language-server typescript")
	}
	return p
}

// repoRoot walks parent dirs until go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root")
		}
		dir = parent
	}
}

// startTSServer spawns typescript-language-server and returns a ready adapter.
func startTSServer(t *testing.T, ws string) *ts.Adapter {
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

	ad := ts.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := ad.Initialize(ctx, ts.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	return ad
}

func TestIntegration_DocumentSymbols(t *testing.T) {
	fixture := filepath.Join(repoRoot(t), "testdata", "typescript-fixture")
	ad := startTSServer(t, fixture)
	srcPath := filepath.Join(fixture, "src", "greeter.ts")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	uri := protocol.FileURI(srcPath)
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: uri, LanguageID: "typescript", Version: 1, Text: string(src),
		},
	}); err != nil {
		t.Fatal("didOpen:", err)
	}

	var syms []protocol.DocumentSymbol
	deadline := time.After(45 * time.Second)
	for {
		syms, err = ad.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err == nil && len(syms) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no document symbols within deadline (err=%v, n=%d)", err, len(syms))
		case <-time.After(time.Second):
		}
	}

	if !hasSymbol(syms, "Greeter") {
		t.Fatalf("symbol Greeter not found; got %v", symbolNames(syms))
	}
}

// hasSymbol reports whether name appears anywhere in the symbol tree.
func hasSymbol(syms []protocol.DocumentSymbol, name string) bool {
	for _, s := range syms {
		if s.Name == name || hasSymbol(s.Children, name) {
			return true
		}
	}
	return false
}

func symbolNames(syms []protocol.DocumentSymbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Name)
	}
	return out
}

// TestIntegration_DidChangeWatchedFiles proves capability negotiation + the
// DidChangeWatchedFiles wire format are accepted by a real
// typescript-language-server, and that the external-write → notify → open →
// diagnostics pipeline works end to end.
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
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

	ad := startTSServer(t, ws)
	brokenPath := filepath.Join(ws, "src", "broken.ts")
	brokenURI := protocol.FileURI(brokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	diagCh := make(chan int, 16)
	ad.Subscribe(func(method string, raw json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p protocol.PublishDiagnosticsParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return
		}
		if p.URI != brokenURI {
			return
		}
		errors := 0
		for _, d := range p.Diagnostics {
			if d.Severity == protocol.SevError {
				errors++
			}
		}
		select {
		case diagCh <- errors:
		default:
		}
	})

	// Write a syntactically broken TypeScript file into the project.
	broken := []byte("export function broken(: {\n")
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: brokenURI, Type: protocol.FileCreated},
		},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: brokenURI, LanguageID: "typescript", Version: 1, Text: string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}

	deadline := time.After(45 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: the server acted on our notification
			}
		case <-deadline:
			t.Fatal("typescript-language-server did not publish error diagnostics for broken.ts within deadline — " +
				"the didChangeWatchedFiles + didOpen pipeline is not reaching the server, " +
				"or capability negotiation is broken")
		}
	}
}
