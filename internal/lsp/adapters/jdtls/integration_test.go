//go:build integration

package jdtls_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/adapters/jdtls"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// requireJDTLS skips if jdtls is not on PATH and returns its path.
func requireJDTLS(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("jdtls")
	if err != nil {
		t.Skip("jdtls not found on PATH — install jdtls (Java 21+ required) and ensure it is on PATH")
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

// startJDTLS spawns jdtls and returns a ready adapter. The process is killed
// via t.Cleanup.
//
// jdtls requires a -data argument pointing to an Eclipse workspace storage
// directory. By default a shared directory under .testcache/jdtls-data is
// reused across runs to avoid the 60-120 s cold-start JVM penalty during local
// iteration. Set JDTLS_FRESH_DATA=1 to use a per-test temp dir instead (fully
// hermetic but slow on cold start).
//
// jdtls stderr is captured to a temp file; its path is logged via t.Logf and
// visible in the test output when the test fails.
func startJDTLS(t *testing.T) *jdtls.Adapter {
	t.Helper()
	jdtlsPath := requireJDTLS(t)

	var dataDir string
	if os.Getenv("JDTLS_FRESH_DATA") == "1" {
		dataDir = t.TempDir()
	} else {
		dataDir = filepath.Join(repoRoot(t), ".testcache", "jdtls-data")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatal("create jdtls data dir:", err)
		}
	}

	stderrFile, err := os.CreateTemp(t.TempDir(), "jdtls-stderr-*.log")
	if err != nil {
		t.Fatal("create stderr log:", err)
	}
	t.Cleanup(func() { stderrFile.Close() })
	t.Logf("jdtls stderr: %s", stderrFile.Name())

	cmd := exec.Command(jdtlsPath, "-data", dataDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("stdin pipe:", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("stdout pipe:", err)
	}
	cmd.Stderr = stderrFile
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

// jdtlsTestDuration returns def, or the duration parsed from the named env var
// when it is set and valid. It lets a slow/cold CI runner widen the cold-start
// budgets without editing the test; an unparseable value is logged and the
// default used.
func jdtlsTestDuration(t *testing.T, env string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Logf("%s=%q is not a valid duration (%v); using default %s", env, v, err, def)
		return def
	}
	return d
}

// TestIntegration_DidOpen verifies that jdtls publishes error diagnostics for
// a syntactically broken Java file opened via DidOpen. The flow:
//
//  1. Copy the java-fixture workspace to a temp directory.
//  2. Initialize jdtls against the workspace.
//  3. Wait for jdtls to signal ServiceReady (fully initialised, project loaded).
//  4. Write Broken.java to disk after ServiceReady so jdtls sees it fresh.
//  5. Send DidChangeWatchedFiles + DidOpen to register and open the new file.
//  6. Wait up to 2 minutes for publishDiagnostics to fire with at least one error.
//
// DidOpen must be sent AFTER ServiceReady. Sending it earlier causes jdtls to
// compile without routing results to textDocument/publishDiagnostics — because
// jdtls blocks that path on the client/registerCapability round-trip that
// arrives at ServiceReady time.
//
// The conn fix that makes this work: jdtls sends client/registerCapability with
// a string ID ("1") rather than an integer ID. The JSON-RPC conn previously
// decoded ID as *int64, which failed for string values and killed the read
// loop. With ID now decoded as json.RawMessage the round-trip completes and
// jdtls proceeds to publish diagnostics.
//
// jdtls starts a JVM and loads Eclipse plugins on each cold run; the 5-minute
// ServiceReady budget (and 2-minute diagnostics budget) covers cold-cache JVM
// startup on typical developer hardware. Subsequent runs reuse the data dir
// under .testcache and are much faster (set JDTLS_FRESH_DATA=1 to force a
// hermetic per-test data dir — fully isolated but pays the cold start every
// time, so prefer the warm cache locally and reserve fresh data for CI runs
// that must not leak state between jobs).
//
// On a slow/cold CI runner these budgets can be tight. Raise them without a code
// change via PLUMB_TEST_JDTLS_READY_TIMEOUT / PLUMB_TEST_JDTLS_DIAG_TIMEOUT
// (Go duration strings, e.g. "8m"); see jdtlsTestDuration.
func TestIntegration_DidOpen(t *testing.T) {
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "java-fixture")

	// Copy fixture to a temp workspace so mutations don't dirty the real testdata.
	ws := t.TempDir()
	if err := copyFixture(t, fixtureSrc, ws); err != nil {
		t.Fatal("copy fixture:", err)
	}

	// Resolve symlinks so jdtls (Java, which canonicalises paths) and our URI
	// filter see the same path. On macOS /var/ → /private/var/.
	realWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		realWS = ws
	}

	brokenPath := filepath.Join(realWS, "src", "main", "java", "com", "example", "Broken.java")
	brokenURI := protocol.FileURI(brokenPath)
	wsURI := protocol.FileURI(realWS)
	t.Logf("wsURI=%s brokenURI=%s", wsURI, brokenURI)

	ad := startJDTLS(t)

	ctx, cancel := context.WithTimeout(context.Background(), jdtlsTestDuration(t, "PLUMB_TEST_JDTLS_READY_TIMEOUT", 5*time.Minute))
	defer cancel()

	// readyCh is closed once jdtls sends ServiceReady. sync.Once guards against
	// the (unlikely but possible) case where two ServiceReady notifications race
	// in separate goroutines and both try to close the channel.
	readyCh := make(chan struct{})
	var readyOnce sync.Once

	// Subscribe before Initialize so we don't miss notifications.
	// All notifications are logged with full payload for post-mortem analysis.
	// publishDiagnostics for Broken.java are forwarded to diagCh regardless of
	// exact URI format (jdtls internally uses file:/ while we send file:///).
	diagCh := make(chan int, 16)
	ad.Subscribe(func(method string, raw json.RawMessage) {
		t.Logf("notification: method=%s payload=%s", method, string(raw))

		if method == "language/status" {
			var status struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &status) == nil && status.Type == "ServiceReady" {
				readyOnce.Do(func() { close(readyCh) })
			}
			return
		}

		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p protocol.PublishDiagnosticsParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return
		}
		if !strings.HasSuffix(p.URI, "Broken.java") {
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

	if _, err := ad.Initialize(ctx, jdtls.DefaultInitParams(wsURI)); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}

	// Wait for jdtls to finish loading the project before writing or opening the file.
	// jdtls sends client/registerCapability at ServiceReady time; until that
	// round-trip completes it does not route publishDiagnostics to the wire.
	t.Log("waiting for jdtls ServiceReady...")
	select {
	case <-readyCh:
		t.Log("jdtls ServiceReady")
	case <-ctx.Done():
		t.Fatal("context expired waiting for jdtls ServiceReady; see t.Logf notifications above and the jdtls stderr log for details")
	}

	// Write Broken.java now (after ServiceReady) so jdtls sees it fresh and is
	// guaranteed to publish diagnostics rather than suppress a duplicate.
	brokenDir := filepath.Dir(brokenPath)
	if err := os.MkdirAll(brokenDir, 0o755); err != nil {
		t.Fatal("mkdir:", err)
	}
	broken := []byte("package com.example;\npublic class Broken {\n    public void broken(\n") // unclosed signature
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	// Inform jdtls the file was created on disk, then open it to trigger reconcile.
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: brokenURI, Type: protocol.FileCreated},
		},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        brokenURI,
			LanguageID: "java",
			Version:    1,
			Text:       string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}
	defer func() {
		_ = ad.DidClose(ctx, protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: brokenURI},
		})
	}()

	// Wait for jdtls to publish diagnostics for Broken.java.
	deadline := time.After(jdtlsTestDuration(t, "PLUMB_TEST_JDTLS_DIAG_TIMEOUT", 2*time.Minute))
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // success: jdtls reported errors for the broken file
			}
			t.Logf("publishDiagnostics for Broken.java received but had 0 errors — waiting for non-zero")
		case <-deadline:
			t.Fatal("jdtls did not publish error diagnostics for Broken.java within 2 minutes of ServiceReady — " +
				"check that DidOpen reaches the server; " +
				"see t.Logf notifications above and the jdtls stderr log for details")
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
