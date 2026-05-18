//go:build integration

package jdtls_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/adapters/jdtls"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// requireJDTLS skips if jdtls is not on PATH and returns its path.
func requireJDTLS(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("jdtls")
	if err != nil {
		t.Skip("jdtls not found on PATH — install with: brew install jdtls")
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

// startJDTLS spawns jdtls with a fresh -data directory and returns a ready adapter.
// The adapter and process are cleaned up via t.Cleanup.
//
// jdtls requires a -data argument pointing to an Eclipse workspace storage
// directory. A new temp dir is created for each test run to avoid stale state.
func startJDTLS(t *testing.T) *jdtls.Adapter {
	t.Helper()
	jdtlsPath := requireJDTLS(t)
	dataDir := t.TempDir()

	cmd := exec.Command(jdtlsPath, "-data", dataDir)
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
		t.Fatal("start jdtls:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })
	return jdtls.New(conn)
}

// TestIntegration_DidChangeWatchedFiles exercises the LSP-correct primitive for
// telling jdtls about external file changes. The flow:
//
//  1. Initialize against the java-fixture workspace.
//  2. Write a syntactically broken .java file using ordinary os.WriteFile
//     (simulating an external edit from plumb's write tools).
//  3. Send DidChangeWatchedFiles{FileCreated}.
//  4. Wait up to 60 seconds for publishDiagnostics to fire with at least one error.
//
// jdtls starts a JVM and loads Eclipse plugins on each run; the 60-second
// budget covers cold-cache startup on typical developer hardware.
//
// This mirrors the same test in the gopls and pyright adapters and proves the
// architectural guarantee: capability negotiation + DidChangeWatchedFiles is
// the mechanism that keeps jdtls's view of the workspace fresh after
// plumb-initiated writes.
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
	ad := startJDTLS(t)
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "java-fixture")

	// Copy the fixture into a temp workspace so we can mutate without dirtying
	// the real testdata directory.
	ws := t.TempDir()
	if err := copyFixture(t, fixtureSrc, ws); err != nil {
		t.Fatal("copy fixture:", err)
	}

	brokenPath := filepath.Join(ws, "src", "main", "java", "com", "example", "Broken.java")
	brokenURI := protocol.FileURI(brokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Subscribe before init so we don't miss early publishDiagnostics bursts.
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

	if _, err := ad.Initialize(ctx, jdtls.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	// Write a syntactically broken Java file into the workspace.
	brokenDir := filepath.Dir(brokenPath)
	if err := os.MkdirAll(brokenDir, 0o755); err != nil {
		t.Fatal("mkdir:", err)
	}
	broken := []byte("package com.example;\npublic class Broken {\n    public void broken(\n") // unclosed signature
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	// Tell jdtls about it via the LSP-correct primitive.
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: brokenURI, Type: protocol.FileCreated},
		},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}

	// Wait up to 60s for jdtls to publish diagnostics for the broken file.
	// jdtls starts a JVM on first run; 60s gives headroom for cold caches.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: jdtls acted on our notification
			}
		case <-deadline:
			t.Fatal("jdtls did not publish error diagnostics for Broken.java within 60s — " +
				"DidChangeWatchedFiles may not be reaching the server, or capability " +
				"negotiation is broken")
		}
	}
}

// copyFixture recursively copies src into dst, creating directories as needed.
func copyFixture(t *testing.T, src, dst string) error {
	t.Helper()
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}
