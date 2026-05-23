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

// TestIntegration_DidChangeWatchedFiles exercises the LSP-correct primitive for
// telling pyright about external file changes. The flow:
//
//  1. Initialize against the python-fixture workspace.
//  2. Write a syntactically broken .py file using ordinary os.WriteFile
//     (simulating an external edit from plumb's write tools).
//  3. Send DidChangeWatchedFiles{FileCreated}.
//  4. Wait up to 15 seconds for publishDiagnostics to fire with at least one error.
//
// This mirrors TestIntegration_DidChangeWatchedFiles in the gopls adapter and
// proves the same architectural guarantee: capability negotiation +
// DidChangeWatchedFiles is the mechanism that keeps pyright's view of the
// workspace fresh after plumb-initiated writes.
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	// Tell pyright about it via the LSP-correct primitive.
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: brokenURI, Type: protocol.FileCreated},
		},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}

	// Wait for pyright to publish diagnostics for the broken file. A freshly
	// installed pyright on a cold CI runner takes far longer than a warm local
	// one to boot Node, initialise, index, and publish — so the deadline is
	// generous (well within the 60 s context above). A healthy pyright still
	// answers in a few seconds; the headroom only covers a cold start.
	deadline := time.After(45 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: pyright acted on our notification
			}
		case <-deadline:
			t.Fatal("pyright did not publish error diagnostics for broken.py within 45s — " +
				"DidChangeWatchedFiles may not be reaching the server, or capability " +
				"negotiation is broken")
		}
	}
}
