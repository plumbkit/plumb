//go:build integration

package pyright_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/adapters/pyright"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// requirePyright skips if pyright-langserver is not on PATH and returns its path.
func requirePyright(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("pyright-langserver")
	if err != nil {
		t.Skip("pyright-langserver not found on PATH — install with: npm install -g pyright")
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

// startPyright spawns pyright-langserver and returns a ready adapter.
// The adapter and process are cleaned up via t.Cleanup.
func startPyright(t *testing.T) *pyright.Adapter {
	t.Helper()
	pyrightPath := requirePyright(t)

	cmd := exec.Command(pyrightPath, "--stdio")
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
		t.Fatal("start pyright-langserver:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })
	return pyright.New(conn)
}

// TestIntegration_DidChangeWatchedFiles exercises the pipeline plumb uses to
// surface a freshly-written file's problems through a real pyright. The flow:
//
//  1. Initialize against the python-fixture workspace.
//  2. Write a syntactically broken .py file using ordinary os.WriteFile
//     (simulating an external edit from plumb's write tools).
//  3. Send DidChangeWatchedFiles{FileCreated} — the LSP-correct primitive that
//     tells pyright about the on-disk change and keeps its view fresh.
//  4. DidOpen the file, then wait for publishDiagnostics with at least one error.
//
// The DidOpen step is essential, not incidental: pyright runs in its default
// "openFilesOnly" diagnostic mode and only publishes diagnostics for open
// documents, so a watched-file notification alone never surfaces an unopened
// file's errors. plumb's diagnostics tool opens files for exactly this reason
// (see internal/tools/diagnostics.go), so this mirrors the production path. The
// test proves capability negotiation + the DidChangeWatchedFiles wire format
// are accepted by a real pyright, and that the external-write → notify → open
// → diagnostics pipeline works end to end. (gopls, by contrast, analyses the
// whole package and surfaces the error from the watched-file event alone — see
// the gopls adapter's equivalent test.)
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
	ad := startPyright(t)
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "python-fixture")

	// Copy the fixture into a temp workspace so we can mutate without dirtying
	// the real testdata directory.
	ws := t.TempDir()
	for _, name := range []string{"pyproject.toml", "main.py"} {
		src, err := os.ReadFile(filepath.Join(fixtureSrc, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	brokenPath := filepath.Join(ws, "broken.py")
	brokenURI := protocol.FileURI(brokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Subscribe to publishDiagnostics BEFORE init so we don't miss any.
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

	if _, err := ad.Initialize(ctx, pyright.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	// Write a syntactically broken Python file into the workspace.
	broken := []byte("def broken(\n") // unclosed parenthesis — syntax error
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	// Tell pyright about the on-disk change via the LSP-correct primitive. This
	// keeps pyright's workspace view fresh; capability negotiation and the wire
	// format are exercised here against a real server.
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: brokenURI, Type: protocol.FileCreated},
		},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}

	// Open the file so pyright (openFilesOnly mode) actually analyses it — the
	// same step plumb's diagnostics tool performs. Without this, pyright never
	// reports on an unopened file regardless of watched-file notifications.
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: brokenURI, LanguageID: "python", Version: 1, Text: string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}

	// Wait for pyright to publish error diagnostics. A healthy pyright answers
	// within a couple of seconds of the open; the generous deadline (well inside
	// the 45 s context) only covers a freshly-installed server cold-starting on
	// a loaded CI runner.
	deadline := time.After(30 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: pyright acted on our notification
			}
		case <-deadline:
			t.Fatal("pyright did not publish error diagnostics for broken.py within 30s — " +
				"the didChangeWatchedFiles + didOpen pipeline is not reaching the server, " +
				"or capability negotiation is broken")
		}
	}
}
