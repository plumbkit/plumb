//go:build integration

package kotlin_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/adapters/kotlin"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// requireKotlinLS skips if kotlin-language-server is not on PATH and returns its
// path. It is not installed on the validation machine, so this test skips there;
// it runs and validates the adapter wherever the binary is present.
func requireKotlinLS(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("kotlin-language-server")
	if err != nil {
		t.Skip("kotlin-language-server not found on PATH — install with: brew install kotlin-language-server")
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

// startKotlinLS spawns kotlin-language-server and returns a ready adapter.
func startKotlinLS(t *testing.T, ws string) *kotlin.Adapter {
	t.Helper()
	bin := requireKotlinLS(t)

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
		t.Fatal("start kotlin-language-server:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })

	ad := kotlin.New(conn)
	// kotlin-language-server resolves a Gradle/Maven classpath on first attach,
	// which can be slow; the generous deadline tolerates a cold start.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := ad.Initialize(ctx, kotlin.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	return ad
}

func copyKotlinFixture(t *testing.T) string {
	t.Helper()
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "kotlin-fixture")
	ws := t.TempDir()
	srcDir := filepath.Join(ws, "src", "main", "kotlin")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copies := map[string]string{
		"build.gradle.kts": filepath.Join(ws, "build.gradle.kts"),
		filepath.Join("src", "main", "kotlin", "Greeter.kt"): filepath.Join(srcDir, "Greeter.kt"),
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
	return ws
}

func TestIntegration_DocumentSymbols(t *testing.T) {
	fixture := copyKotlinFixture(t)
	ad := startKotlinLS(t, fixture)
	srcPath := filepath.Join(fixture, "src", "main", "kotlin", "Greeter.kt")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	uri := protocol.FileURI(srcPath)
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: uri, LanguageID: "kotlin", Version: 1, Text: string(src),
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
		case <-time.After(2 * time.Second):
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
// kotlin-language-server, and that the external-write → notify → open →
// diagnostics pipeline works end to end.
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
	ws := copyKotlinFixture(t)
	srcDir := filepath.Join(ws, "src", "main", "kotlin")
	ad := startKotlinLS(t, ws)
	brokenPath := filepath.Join(srcDir, "Broken.kt")
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

	// Write a syntactically broken Kotlin file (unterminated fun) into the module.
	broken := []byte("package fixture\n\nfun broken(\n")
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
			URI: brokenURI, LanguageID: "kotlin", Version: 1, Text: string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}

	deadline := time.After(90 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: the server acted on our notification
			}
		case <-deadline:
			t.Fatal("kotlin-language-server did not publish error diagnostics for Broken.kt within deadline — " +
				"the didChangeWatchedFiles + didOpen pipeline is not reaching the server, " +
				"or capability negotiation is broken")
		}
	}
}
