package cli

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// sendCtrl dials a net.Listener (already started), sends a command, and
// returns the trimmed single-line response.
func sendCtrl(t *testing.T, ln net.Listener, cmd string) string {
	t.Helper()
	conn, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimRight(resp, "\n")
}

func testCtrlListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// These tests mutate the process-global slog.Default(); must NOT be t.Parallel().

func TestHandleCtrlConn_SetLevel(t *testing.T) {
	t.Cleanup(func() { _ = setupLogging("info", "text") })

	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", nil)

	resp := sendCtrl(t, ln, "set-level debug")
	if resp != "ok" {
		t.Fatalf("expected ok, got %q", resp)
	}
	if !slog.Default().Enabled(context.TODO(), slog.LevelDebug) {
		t.Error("debug level should be enabled after set-level debug")
	}
}

func TestHandleCtrlConn_Reset(t *testing.T) {
	t.Cleanup(func() { _ = setupLogging("info", "text") })

	_ = setupLogging("debug", "text") // start at debug

	ln := testCtrlListener(t)
	go serveControlSocket(ln, "warn", "text", nil) // configLevel = warn

	resp := sendCtrl(t, ln, "set-level reset")
	if resp != "ok" {
		t.Fatalf("expected ok, got %q", resp)
	}
	if slog.Default().Enabled(context.TODO(), slog.LevelInfo) {
		t.Error("info should NOT be enabled after reset to warn")
	}
}

func TestHandleCtrlConn_InvalidLevel(t *testing.T) {
	t.Cleanup(func() { _ = setupLogging("info", "text") })

	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", nil)

	resp := sendCtrl(t, ln, "set-level nonsense")
	if !strings.HasPrefix(resp, "error:") {
		t.Fatalf("expected error response, got %q", resp)
	}
}

func TestHandleCtrlConn_UnknownCommand(t *testing.T) {
	t.Cleanup(func() { _ = setupLogging("info", "text") })

	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", nil)

	resp := sendCtrl(t, ln, "get-level")
	if !strings.HasPrefix(resp, "error:") {
		t.Fatalf("expected error response for unknown command, got %q", resp)
	}
}

// sendCtrlAll dials, sends a command, and reads the full response until EOF.
// Used for commands like "diagnostics" whose response spans multiple lines.
func sendCtrlAll(t *testing.T, ln net.Listener, cmd string) string {
	t.Helper()
	conn, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestHandleCtrlConn_Diagnostics(t *testing.T) {
	ln := testCtrlListener(t)
	called := false
	diagsFn := func(workspace string) string {
		called = true
		if workspace != "/my/project" {
			t.Errorf("unexpected workspace %q", workspace)
		}
		return "1 issue(s) across 1 file(s)\n\n/my/project/main.go\n  ERROR  1:1  undefined: foo\n"
	}
	go serveControlSocket(ln, "info", "text", diagsFn)

	resp := sendCtrlAll(t, ln, "diagnostics /my/project")
	if !called {
		t.Fatal("diagsFn was not called")
	}
	if !strings.Contains(resp, "undefined: foo") {
		t.Fatalf("unexpected response: %q", resp)
	}
}

func TestHandleCtrlConn_DiagnosticsNilFn(t *testing.T) {
	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text", nil)
	// nil diagsFn should return empty response without panic.
	resp := sendCtrlAll(t, ln, "diagnostics /any/path")
	if resp != "" {
		t.Fatalf("expected empty response with nil diagsFn, got %q", resp)
	}
}

func TestRunLogLevel_InvalidLevelRejectedBeforeDial(t *testing.T) {
	err := runLogLevel(nil, []string{"nonsense"})
	if err == nil {
		t.Fatal("expected invalid level error")
	}
	if !strings.Contains(err.Error(), "invalid log level") {
		t.Fatalf("expected invalid log level error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "daemon control socket unavailable") {
		t.Fatalf("invalid level should be rejected before dialing daemon, got %q", err.Error())
	}
}
