package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// frameNotification writes one Content-Length-framed JSON-RPC notification.
func frameNotification(w io.Writer, method string) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q}`, method)
	_, _ = fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body)
}

// TestConn_NotificationsDeliveredInOrder verifies server notifications reach the
// handler in wire order. The old code spawned a goroutine per notification, so
// e.g. an empty-then-real publishDiagnostics pair for one URI could be reordered
// and leave the stale set winning. Regression test for lsp-2.
func TestConn_NotificationsDeliveredInOrder(t *testing.T) {
	dr, dw := io.Pipe()
	_, cw := io.Pipe()
	conn := NewConn(dr, cw)
	defer conn.Close()
	defer dw.Close()

	const n = 50
	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	conn.SetNotificationHandler(func(method string, _ json.RawMessage) {
		mu.Lock()
		got = append(got, method)
		if len(got) == n {
			close(done)
		}
		mu.Unlock()
	})

	go func() {
		for i := 0; i < n; i++ {
			frameNotification(dw, fmt.Sprintf("m%03d", i))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notifications")
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("m%03d", i); got[i] != want {
			t.Fatalf("notification %d out of order: got %q, want %q", i, got[i], want)
		}
	}
}

// TestConn_WriteStallClosesConn verifies a write that stalls (server stops
// draining its stdin) trips the watchdog: the connection is closed so the call
// returns a clear error instead of wedging forever on the held write lock.
// Regression test for lsp-1.
func TestConn_WriteStallClosesConn(t *testing.T) {
	old := writeStallTimeout
	writeStallTimeout = 50 * time.Millisecond
	defer func() { writeStallTimeout = old }()

	dr, dw := io.Pipe()
	_, cw := io.Pipe() // no reader → the conn's write blocks forever
	conn := NewConn(dr, cw)
	defer dw.Close()

	// No ctx deadline: only the stall watchdog can unblock this.
	err := conn.Notify(context.Background(), "test/stall", nil)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected a write-stall error, got %v", err)
	}
	// The connection must be closed now, so further calls fail fast.
	select {
	case <-conn.done:
	default:
		t.Fatal("connection was not closed after a stalled write")
	}
}
