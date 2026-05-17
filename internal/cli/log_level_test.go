package cli

import (
	"bufio"
	"context"
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
	go serveControlSocket(ln, "info", "text")

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
	go serveControlSocket(ln, "warn", "text") // configLevel = warn

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
	go serveControlSocket(ln, "info", "text")

	resp := sendCtrl(t, ln, "set-level nonsense")
	if !strings.HasPrefix(resp, "error:") {
		t.Fatalf("expected error response, got %q", resp)
	}
}

func TestHandleCtrlConn_UnknownCommand(t *testing.T) {
	t.Cleanup(func() { _ = setupLogging("info", "text") })

	ln := testCtrlListener(t)
	go serveControlSocket(ln, "info", "text")

	resp := sendCtrl(t, ln, "get-level")
	if !strings.HasPrefix(resp, "error:") {
		t.Fatalf("expected error response for unknown command, got %q", resp)
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
