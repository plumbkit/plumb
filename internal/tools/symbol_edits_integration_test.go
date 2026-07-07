//go:build integration

package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// startGoplsClient spawns a real gopls and returns it as an lsp.Client already
// initialised against rootDir. Cleaned up via t.Cleanup.
func startGoplsClient(t *testing.T, rootDir string) lsp.Client {
	t.Helper()
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not found on PATH")
	}
	cmd := exec.Command(goplsPath, "serve")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("stdin pipe:", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("stdout pipe:", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal("start gopls:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })

	var client lsp.Client = gopls.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(rootDir))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := client.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	return client
}

// runReplaceUntil retries replace_symbol_body until it applies without error or
// the deadline passes. A failed attempt (e.g. a stale-range "out of range")
// leaves the file untouched, so retrying is safe; this both tolerates gopls's
// initial package-load latency and — crucially for RC1 — waits for gopls to
// process the previous edit's didChangeWatchedFiles notification before the next
// documentSymbol resolves. Without the RC1 fix that notification is never sent,
// so the retry never succeeds and the caller fails.
func runReplaceUntil(t *testing.T, tool *tools.ReplaceSymbolBody, args json.RawMessage, deadline time.Time) (string, error) {
	t.Helper()
	var (
		out     string
		lastErr error
	)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, lastErr = tool.Execute(ctx, args)
		cancel()
		if lastErr == nil {
			return out, nil
		}
		if time.Now().After(deadline) {
			return out, lastErr
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func replaceArgs(t *testing.T, uri, namePath, content string) json.RawMessage {
	t.Helper()
	dry := false
	b, err := json.Marshal(map[string]any{
		"uri": uri, "name_path": namePath, "content": content, "dry_run": &dry,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIntegration_ChainedReplaceSymbolBody is the RC1 end-to-end repro: two
// replace_symbol_body calls on the SAME file, where the first SHRINKS an earlier
// symbol so a stale server view would place the second symbol's range past the
// new end-of-file. Before the fix the second edit fails with a stale-range
// "position out of range" (gopls never learned the file changed); after the fix
// the first edit notifies gopls, so the second resolves fresh and applies.
func TestIntegration_ChainedReplaceSymbolBody(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module chained\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// funcA spans many lines; shrinking it moves funcB (the last symbol) up far
	// enough that a stale funcB range would point past the new EOF.
	src := "package chained\n\n" +
		"func funcA() int {\n" +
		"\tx := 0\n\tx++\n\tx++\n\tx++\n\tx++\n\tx++\n\tx++\n\tx++\n\treturn x\n" +
		"}\n\n" +
		"func funcB() int {\n\treturn 1\n}\n"
	path := filepath.Join(ws, "main.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := protocol.FileURI(path)

	client := startGoplsClient(t, ws)
	tool := tools.NewReplaceSymbolBody(client, 30*time.Second)

	deadline := time.Now().Add(30 * time.Second)

	// Edit 1: shrink funcA from ~11 lines to one.
	if _, err := runReplaceUntil(t, tool, replaceArgs(t, uri, "funcA", "func funcA() int { return 0 }"), deadline); err != nil {
		t.Fatalf("first replace_symbol_body failed: %v", err)
	}

	// Edit 2: replace funcB. Before the RC1 fix this fails because gopls still
	// holds funcB at its old (higher) line range.
	if _, err := runReplaceUntil(t, tool, replaceArgs(t, uri, "funcB", "func funcB() int { return 2 }"), deadline); err != nil {
		t.Fatalf("second replace_symbol_body failed (RC1 staleness): %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	final := string(got)
	if !strings.Contains(final, "func funcA() int { return 0 }") {
		t.Errorf("funcA edit missing from final content:\n%s", final)
	}
	if !strings.Contains(final, "func funcB() int { return 2 }") {
		t.Errorf("funcB edit missing from final content:\n%s", final)
	}
	if strings.Contains(final, "x++") {
		t.Errorf("funcA body not replaced:\n%s", final)
	}
	if strings.Contains(final, "return 1") {
		t.Errorf("funcB body not replaced (stale range applied):\n%s", final)
	}
}
