//go:build integration

package swift_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/adapters/swift"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// requireSourceKitLSP skips if sourcekit-lsp is not on PATH and returns its path.
func requireSourceKitLSP(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sourcekit-lsp")
	if err != nil {
		t.Skip("sourcekit-lsp not found on PATH — install the Swift toolchain (Xcode or swift.org)")
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

// startSourceKitLSP spawns sourcekit-lsp and returns a ready adapter against ws.
// The adapter and process are cleaned up via t.Cleanup.
func startSourceKitLSP(t *testing.T, ws string) *swift.Adapter {
	t.Helper()
	bin := requireSourceKitLSP(t)

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
		t.Fatal("start sourcekit-lsp:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })

	ad := swift.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := ad.Initialize(ctx, swift.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	return ad
}

// copyFixture copies the swift-fixture SwiftPM package into ws.
func copyFixture(t *testing.T, ws string) {
	t.Helper()
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "swift-fixture")
	srcDir := filepath.Join(ws, "Sources", "SwiftFixture")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Package.swift": filepath.Join(ws, "Package.swift"),
		filepath.Join("Sources", "SwiftFixture", "Greeter.swift"): filepath.Join(srcDir, "Greeter.swift"),
	}
	for rel, dst := range files {
		src, err := os.ReadFile(filepath.Join(fixtureSrc, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntegration_DocumentSymbols(t *testing.T) {
	ws := t.TempDir()
	copyFixture(t, ws)
	ad := startSourceKitLSP(t, ws)
	greeterPath := filepath.Join(ws, "Sources", "SwiftFixture", "Greeter.swift")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	src, err := os.ReadFile(greeterPath)
	if err != nil {
		t.Fatal(err)
	}
	uri := protocol.FileURI(greeterPath)
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: uri, LanguageID: "swift", Version: 1, Text: string(src),
		},
	}); err != nil {
		t.Fatal("didOpen:", err)
	}

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
// DidChangeWatchedFiles wire format are accepted by a real sourcekit-lsp, and
// that the external-write → notify → open → diagnostics pipeline works end to
// end. A syntactically broken Swift file is written into the package's source
// directory, announced via the LSP-correct primitive, and opened; sourcekit-lsp
// reports the parse error from its Swift front end.
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
	ws := t.TempDir()
	copyFixture(t, ws)
	ad := startSourceKitLSP(t, ws)

	brokenPath := filepath.Join(ws, "Sources", "SwiftFixture", "Broken.swift")
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

	// Write a syntactically broken Swift file (unterminated func) into the target.
	broken := []byte("public func broken( {\n")
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
			URI: brokenURI, LanguageID: "swift", Version: 1, Text: string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}

	deadline := time.After(90 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: sourcekit-lsp acted on our notification
			}
		case <-deadline:
			t.Fatal("sourcekit-lsp did not publish error diagnostics for Broken.swift within deadline — " +
				"the didChangeWatchedFiles + didOpen pipeline is not reaching the server, " +
				"or capability negotiation is broken")
		}
	}
}
