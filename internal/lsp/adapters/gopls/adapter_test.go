//go:build integration

package gopls_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
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

// goFixtureWS copies testdata/go-fixture (go.mod + main.go) into a fresh temp
// workspace and returns its path. Every test here goes through it.
//
// Using the fixture IN PLACE does not work, and fails in a way that looks like
// a broken adapter rather than a broken fixture. testdata/go-fixture declares
// its own module, but it sits inside this repo — so when this repo is checked
// out beneath a go.work that `use`s it (plumb's own ops workspace does exactly
// that), the workspace file wins and the fixture resolves to the PARENT module.
// Go then excludes it anyway, because it lives under testdata/. gopls is left
// serving a directory that belongs to no package, and answers documentSymbol
// and definition with nothing at all.
//
// A temp workspace has no go.work above it, so the fixture's own go.mod
// governs. This also keeps a mutating test from dirtying testdata/.
func goFixtureWS(t *testing.T) string {
	t.Helper()
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "go-fixture")
	ws := t.TempDir()
	for _, name := range []string{"go.mod", "main.go"} {
		src, err := os.ReadFile(filepath.Join(fixtureSrc, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
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
	fixture := goFixtureWS(t)
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
	fixture := goFixtureWS(t)

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
	fixture := goFixtureWS(t)
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

	// Line 16 is `g := Greeter{...}` — jump to Greeter definition.
	locs, err := ad.Definition(ctx, protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 15, Character: 7},
	})
	if err != nil {
		t.Fatal("definition:", err)
	}
	if len(locs) == 0 {
		t.Fatal("expected at least one location")
	}
}

// TestIntegration_DidChangeWatchedFiles exercises the LSP-correct primitive
// for telling gopls about external file changes. The flow:
//
//  1. Initialize against the fixture workspace.
//  2. Wait for gopls to publish initial diagnostics (the fixture is clean,
//     so we expect either empty diagnostics or none at all).
//  3. Write a syntactically broken file to the workspace using ordinary
//     os.WriteFile (simulating an external edit, since plumb's write tools
//     live in the tools package — for this adapter-level test we use the
//     primitive directly).
//  4. Send DidChangeWatchedFiles{FileChanged}.
//  5. Wait up to 5 seconds for publishDiagnostics to fire with at least one
//     error for the broken file.
//
// This is the test that proves the 0.5.x architectural rewrite is
// load-bearing: capability negotiation + DidChangeWatchedFiles is the
// machinery that lets plumb keep gopls's view of the workspace fresh
// after every plumb-initiated write.
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
	ad := startGopls(t)
	ws := goFixtureWS(t)
	brokenPath := filepath.Join(ws, "broken.go")
	brokenURI := protocol.FileURI(brokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Subscribe to publishDiagnostics BEFORE init so we don't miss any.
	diagCh := make(chan int, 16) // sends the error-count for brokenURI each publish
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

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	// Write a syntactically broken Go file into the workspace.
	broken := []byte("package main\n\nfunc broken( { } // missing param/return\n")
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	// Tell gopls about it via the LSP-correct primitive.
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: brokenURI, Type: protocol.FileCreated},
		},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}

	// Wait up to 5s for gopls to publish diagnostics for our broken file.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: gopls acted on our notification
			}
		case <-deadline:
			t.Fatal("gopls did not publish error diagnostics for broken.go within 5s — " +
				"DidChangeWatchedFiles may not be reaching the server, or capability " +
				"negotiation is broken")
		}
	}
}
