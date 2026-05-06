//go:build integration

package gopls_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/adapters/gopls"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// requireGopls skips if gopls is not on PATH and returns its path.
func requireGopls(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not found on PATH")
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

// startGopls spawns gopls and returns a ready adapter. The adapter and process
// are cleaned up via t.Cleanup.
func startGopls(t *testing.T) *gopls.Adapter {
	t.Helper()
	goplsPath := requireGopls(t)

	cmd := exec.Command(goplsPath, "serve")
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
		t.Fatal("start gopls:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })
	return gopls.New(conn)
}

func TestIntegration_DocumentSymbols(t *testing.T) {
	ad := startGopls(t)
	fixture := filepath.Join(repoRoot(t), "testdata", "go-fixture")
	mainPath := filepath.Join(fixture, "main.go")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(fixture))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	uri := protocol.FileURI(mainPath)
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: uri, LanguageID: "go", Version: 1, Text: string(src),
		},
	}); err != nil {
		t.Fatal("didOpen:", err)
	}

	syms, err := ad.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		t.Fatal("documentSymbols:", err)
	}
	if len(syms) == 0 {
		t.Fatal("expected symbols, got none")
	}
	found := false
	for _, s := range syms {
		if s.Name == "Greeter" {
			found = true
		}
	}
	if !found {
		names := make([]string, len(syms))
		for i, s := range syms {
			names[i] = s.Name
		}
		t.Fatalf("symbol Greeter not found; got %v", names)
	}
}

func TestIntegration_WorkspaceSymbols(t *testing.T) {
	ad := startGopls(t)
	fixture := filepath.Join(repoRoot(t), "testdata", "go-fixture")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(fixture))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	syms, err := ad.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: "Greet"})
	if err != nil {
		t.Fatal("workspaceSymbols:", err)
	}
	if len(syms) == 0 {
		t.Fatal("expected symbols, got none")
	}
}

func TestIntegration_Definition(t *testing.T) {
	ad := startGopls(t)
	fixture := filepath.Join(repoRoot(t), "testdata", "go-fixture")
	mainPath := filepath.Join(fixture, "main.go")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(fixture))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	src, _ := os.ReadFile(mainPath)
	uri := protocol.FileURI(mainPath)
	_ = ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, LanguageID: "go", Version: 1, Text: string(src)},
	})

	// Line 14 is `g := Greeter{...}` — jump to Greeter definition.
	locs, err := ad.Definition(ctx, protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 13, Character: 7},
	})
	if err != nil {
		t.Fatal("definition:", err)
	}
	if len(locs) == 0 {
		t.Fatal("expected at least one location")
	}
}
