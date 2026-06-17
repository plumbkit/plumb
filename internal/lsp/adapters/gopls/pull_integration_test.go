//go:build integration

package gopls_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// TestIntegration_PullDiagnostics_LiveOrSkip exercises the LSP 3.17 pull path
// against the real gopls binary. gopls is push-first and does NOT advertise
// diagnosticProvider under plumb's negotiated capabilities (we deliberately do
// not advertise the pull *client* capability — advertising it risks a dual-mode
// server switching to pull-only and regressing the validated push path). So
// SupportsPullDiagnostics is expected to report false here and the test SKIPS
// the live pull assertion. When a pull-capable server is in use it asserts the
// report comes back with the broken file's diagnostics.
func TestIntegration_PullDiagnostics_LiveOrSkip(t *testing.T) {
	ad := startGopls(t)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	if !ad.SupportsPullDiagnostics() {
		t.Skip("gopls does not advertise pull diagnostics under plumb's negotiated " +
			"capabilities (the pull client capability is deliberately un-advertised); " +
			"no installed server backs the live pull path — the routing delegation is " +
			"covered by the routingProxy unit tests")
	}

	brokenPath := filepath.Join(ws, "broken.go")
	broken := "package main\n\nfunc broken() int { return \"oops\" }\n"
	if err := os.WriteFile(brokenPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	brokenURI := protocol.FileURI(brokenPath)
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: brokenURI, Type: protocol.FileCreated}},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}

	rep, err := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: brokenURI},
	})
	if err != nil {
		t.Fatalf("pull Diagnostic: %v", err)
	}
	if rep == nil || len(rep.Items) == 0 {
		t.Fatalf("expected pulled diagnostics for the broken file, got %+v", rep)
	}
}

// TestIntegration_PullAdditions_NoPushRegression is the regression guard: the
// pull-diagnostics additions (the adapter Diagnostic/SupportsPullDiagnostics
// methods and the routing delegation) must not disturb the validated push path.
// It mirrors TestIntegration_DidChangeWatchedFiles — a broken file written into
// the workspace and announced via DidChangeWatchedFiles must still produce
// pushed publishDiagnostics. (The pull client capability stays un-advertised, so
// gopls keeps pushing.)
func TestIntegration_PullAdditions_NoPushRegression(t *testing.T) {
	ad := startGopls(t)
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
	brokenPath := filepath.Join(ws, "broken.go")
	brokenURI := protocol.FileURI(brokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		errs := 0
		for _, d := range p.Diagnostics {
			if d.Severity == protocol.SevError {
				errs++
			}
		}
		select {
		case diagCh <- errs:
		default:
		}
	})

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	broken := []byte("package main\n\nfunc broken( { } // missing param/return\n")
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: brokenURI, Type: protocol.FileCreated}},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // push path intact — no regression
			}
		case <-deadline:
			t.Fatal("gopls did not push error diagnostics for broken.go within deadline — " +
				"the pull-diagnostics additions regressed the push path")
		}
	}
}
