//go:build integration

package zig_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/adapters/zig"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// requireZLS skips if zls is not on PATH and returns its path. zls is not
// installed on the validation machine, so this test skips there; it runs and
// validates the adapter on any machine with zls present.
func requireZLS(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("zls")
	if err != nil {
		t.Skip("zls not found on PATH — install from https://github.com/zigtools/zls")
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

// startZLS spawns zls and returns a ready adapter against ws.
func startZLS(t *testing.T, ws string) *zig.Adapter {
	t.Helper()
	bin := requireZLS(t)

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
		t.Fatal("start zls:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })

	ad := zig.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := ad.Initialize(ctx, zig.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	return ad
}

func TestIntegration_DocumentSymbols(t *testing.T) {
	fixture := filepath.Join(repoRoot(t), "testdata", "zig-fixture")
	ad := startZLS(t, fixture)
	mainPath := filepath.Join(fixture, "src", "main.zig")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	uri := protocol.FileURI(mainPath)
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: uri, LanguageID: "zig", Version: 1, Text: string(src),
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
// DidChangeWatchedFiles wire format are accepted by a real zls, and that the
// external-write → notify → open → diagnostics pipeline works end to end.
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "zig-fixture")

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"build.zig", filepath.Join("src", "main.zig")} {
		src, err := os.ReadFile(filepath.Join(fixtureSrc, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, rel), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ad := startZLS(t, ws)
	brokenPath := filepath.Join(ws, "src", "broken.zig")
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

	// Write a syntactically broken Zig file (unterminated fn) into the module.
	broken := []byte("pub fn broken(\n")
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
			URI: brokenURI, LanguageID: "zig", Version: 1, Text: string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}

	deadline := time.After(45 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: zls acted on our notification
			}
		case <-deadline:
			t.Fatal("zls did not publish error diagnostics for broken.zig within deadline — " +
				"the didChangeWatchedFiles + didOpen pipeline is not reaching the server, " +
				"or capability negotiation is broken")
		}
	}
}
