//go:build integration

package rust_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/adapters/rust"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// requireRustAnalyzer skips if rust-analyzer is not on PATH and returns its path.
func requireRustAnalyzer(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("rust-analyzer")
	if err != nil {
		t.Skip("rust-analyzer not found on PATH — install with: rustup component add rust-analyzer")
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

// startRustAnalyzer spawns rust-analyzer and returns a ready adapter against ws.
// The adapter and process are cleaned up via t.Cleanup.
func startRustAnalyzer(t *testing.T, ws string) *rust.Adapter {
	t.Helper()
	bin := requireRustAnalyzer(t)

	cmd := exec.Command(bin)
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
		t.Fatal("start rust-analyzer:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })

	ad := rust.New(conn)
	// rust-analyzer can take a long time to load the sysroot + run cargo
	// metadata; the generous deadline tolerates a cold start.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := ad.Initialize(ctx, rust.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	return ad
}

func TestIntegration_DocumentSymbols(t *testing.T) {
	fixture := filepath.Join(repoRoot(t), "testdata", "rust-fixture")
	ad := startRustAnalyzer(t, fixture)
	libPath := filepath.Join(fixture, "src", "lib.rs")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	src, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatal(err)
	}
	uri := protocol.FileURI(libPath)
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: uri, LanguageID: "rust", Version: 1, Text: string(src),
		},
	}); err != nil {
		t.Fatal("didOpen:", err)
	}

	// rust-analyzer answers documentSymbol from its syntax tree once the file
	// is open; retry briefly while it finishes loading the crate graph.
	var syms []protocol.DocumentSymbol
	deadline := time.After(90 * time.Second)
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
// DidChangeWatchedFiles wire format are accepted by a real rust-analyzer, and
// that the external-write → notify → open → diagnostics pipeline works end to
// end. A syntactically broken Rust file is written into the workspace, then
// announced via the LSP-correct primitive and opened; rust-analyzer reports the
// parse error from its own front end (no cargo check needed).
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "rust-fixture")

	// Copy the fixture into a temp workspace so we can mutate it freely.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	copies := map[string]string{
		"Cargo.toml": filepath.Join(ws, "Cargo.toml"),
		"src/lib.rs": filepath.Join(ws, "src", "lib.rs"),
	}
	for rel, dst := range copies {
		src, err := os.ReadFile(filepath.Join(fixtureSrc, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ad := startRustAnalyzer(t, ws)
	brokenPath := filepath.Join(ws, "src", "broken.rs")
	brokenURI := protocol.FileURI(brokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// Write a syntactically broken Rust file (unclosed paren) into the module.
	broken := []byte("pub fn broken(\n")
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	// Reference it from the crate root so rust-analyzer treats it as a module.
	if err := os.WriteFile(filepath.Join(ws, "src", "lib.rs"),
		[]byte("pub mod broken;\n"), 0o644); err != nil {
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
			URI: brokenURI, LanguageID: "rust", Version: 1, Text: string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}

	deadline := time.After(90 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: rust-analyzer acted on our notification
			}
		case <-deadline:
			t.Fatal("rust-analyzer did not publish error diagnostics for broken.rs within deadline — " +
				"the didChangeWatchedFiles + didOpen pipeline is not reaching the server, " +
				"or capability negotiation is broken")
		}
	}
}
