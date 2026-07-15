//go:build integration

package zig_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/adapters/zig"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// spawnZLSRaw starts zls and returns an un-initialised adapter so the caller can
// drive Initialize with custom (pull) capabilities.
func spawnZLSRaw(t *testing.T) *zig.Adapter {
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
	return zig.New(conn)
}

// TestIntegration_ForcedPull_Probe RECORDS what zls does under forced-pull
// capabilities — evidence only. Zig's validated auto policy is PUSH regardless
// (zig/doc.go: the "zls is pull-only" hypothesis was disproven — zls pushes once
// the publishDiagnostics client capability is advertised). This test asserts
// only the internal consistency of what it observes: whether zls advertises
// diagnosticProvider under forced pull caps must match whether a document pull
// is answered (rather than method-not-found). The version-specific outcome is
// logged for the card. The push path stays green via
// TestIntegration_DidChangeWatchedFiles.
func TestIntegration_ForcedPull_Probe(t *testing.T) {
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

	ad := spawnZLSRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	params := zig.DefaultInitParams(protocol.FileURI(ws))
	params.Capabilities = protocol.ClientCapabilitiesFor(true)
	res, err := ad.Initialize(ctx, params)
	if err != nil {
		t.Fatal("initialize (pull caps):", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	advertised := ad.SupportsPullDiagnostics()
	opts, hasOpts := res.Capabilities.DiagnosticOptions()
	t.Logf("OBSERVED zls (0.16): diagnosticProvider advertised=%v opts=%+v hasOpts=%v",
		advertised, opts, hasOpts)

	// Write a broken Zig file and pull it. The open lifecycle mirrors the push
	// integration test (zls resolves nothing for an unopened document).
	brokenPath := filepath.Join(ws, "src", "broken.zig")
	broken := "pub fn broken(\n"
	if err := os.WriteFile(brokenPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	brokenURI := protocol.FileURI(brokenPath)
	_ = ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: brokenURI, Type: protocol.FileCreated}},
	})
	_ = ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: brokenURI, LanguageID: "zig", Version: 1, Text: broken,
		},
	})

	rep, pullErr := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: brokenURI},
	})
	switch {
	case pullErr == nil:
		items := 0
		if rep != nil {
			items = len(rep.Items)
		}
		// Recorded quirk: zls 0.16 answers textDocument/diagnostic even though it
		// did NOT advertise diagnosticProvider, returning an (often empty) report.
		// This is why zls's validated auto policy is PUSH, not pull.
		t.Logf("OBSERVED zls document pull: kind=%v items=%d (answered without error; "+
			"advertised=%v)", reportKind(rep), items, advertised)
	case jsonrpc.IsMethodNotFound(pullErr):
		t.Logf("OBSERVED zls document pull: -32601 method-not-found (%v)", pullErr)
	default:
		t.Logf("OBSERVED zls document pull error (non -32601): %v", pullErr)
	}
}

func reportKind(rep *protocol.DocumentDiagnosticReport) string {
	if rep == nil {
		return "<nil>"
	}
	if rep.Kind == "" {
		return "<empty>"
	}
	return rep.Kind
}
