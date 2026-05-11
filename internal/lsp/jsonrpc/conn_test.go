package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// frame wraps JSON in an LSP content-length frame.
func frame(v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(b), b)
}

// TestConn_Call_roundtrip verifies that a request gets matched to its response.
func TestConn_Call_roundtrip(t *testing.T) {
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()

	conn := NewConn(pr, cw)
	defer conn.Close()

	// Fake server: read one request, send a response.
	go func() {
		req, _ := readMessage(bufio.NewReader(cr))
		resp := wireMessage{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`"pong"`),
		}
		_, _ = pw.Write([]byte(frame(resp)))
	}()

	var got string
	if err := conn.Call(context.Background(), "ping", nil, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "pong" {
		t.Fatalf("got %q, want %q", got, "pong")
	}
}

// TestConn_Notification verifies server-initiated notifications are dispatched.
func TestConn_Notification(t *testing.T) {
	pr, pw := io.Pipe()
	_, cw := io.Pipe()

	received := make(chan string, 1)
	conn := NewConn(pr, cw)
	conn.SetNotificationHandler(func(method string, _ json.RawMessage) {
		received <- method
	})
	defer conn.Close()

	notif := wireMessage{JSONRPC: "2.0", Method: "window/logMessage", Params: json.RawMessage(`{}`)}
	_, _ = pw.Write([]byte(frame(notif)))

	select {
	case method := <-received:
		if method != "window/logMessage" {
			t.Fatalf("got method %q, want %q", method, "window/logMessage")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}
}

// TestConn_ConcurrentCalls verifies multiple in-flight calls are correctly demuxed.
func TestConn_ConcurrentCalls(t *testing.T) {
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()

	conn := NewConn(pr, cw)
	defer conn.Close()

	const n = 10
	// Fake server: read n requests, answer them in reverse order.
	go func() {
		br := bufio.NewReader(cr)
		reqs := make([]wireMessage, n)
		for i := range n {
			msg, err := readMessage(br)
			if err != nil {
				return
			}
			reqs[i] = msg
		}
		for i := n - 1; i >= 0; i-- {
			resp := wireMessage{
				JSONRPC: "2.0",
				ID:      reqs[i].ID,
				Result:  json.RawMessage(fmt.Sprintf(`%d`, *reqs[i].ID)),
			}
			_, _ = pw.Write([]byte(frame(resp)))
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got int64
			if err := conn.Call(context.Background(), "echo", nil, &got); err != nil {
				t.Errorf("Call error: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestConn_ContextCancel verifies a cancelled context unblocks Call.
func TestConn_ContextCancel(t *testing.T) {
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()
	defer pw.Close()
	defer cr.Close()
	go io.Copy(io.Discard, cr) // drain requests; never sends a response

	conn := NewConn(pr, cw)
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.Call(ctx, "noreply", nil, nil)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error after cancel, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not unblock after context cancel")
	}
}

// TestConn_ServerRequest_OK verifies that a server-initiated request is
// dispatched to the registered handler and the result is sent back.
func TestConn_ServerRequest_OK(t *testing.T) {
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()

	conn := NewConn(pr, cw)
	defer conn.Close()

	conn.SetRequestHandler(func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method != "client/registerCapability" {
			return nil, &MethodNotFoundError{Method: method}
		}
		return nil, nil
	})

	respCh := make(chan wireMessage, 1)
	go func() {
		msg, err := readMessage(bufio.NewReader(cr))
		if err == nil {
			respCh <- msg
		}
	}()

	id := int64(7)
	req := wireMessage{JSONRPC: "2.0", ID: &id, Method: "client/registerCapability", Params: json.RawMessage(`{}`)}
	_, _ = pw.Write([]byte(frame(req)))

	select {
	case resp := <-respCh:
		if resp.ID == nil || *resp.ID != 7 {
			t.Fatalf("response ID = %v, want 7", resp.ID)
		}
		if resp.Error != nil {
			t.Fatalf("unexpected error response: %v", resp.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response")
	}
}

// TestConn_ServerRequest_MethodNotFound verifies an unhandled server request
// gets a -32601 error response.
func TestConn_ServerRequest_MethodNotFound(t *testing.T) {
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()

	conn := NewConn(pr, cw)
	defer conn.Close()
	// No handler registered.

	respCh := make(chan wireMessage, 1)
	go func() {
		msg, err := readMessage(bufio.NewReader(cr))
		if err == nil {
			respCh <- msg
		}
	}()

	id := int64(11)
	req := wireMessage{JSONRPC: "2.0", ID: &id, Method: "weird/thing"}
	_, _ = pw.Write([]byte(frame(req)))

	select {
	case resp := <-respCh:
		if resp.Error == nil || resp.Error.Code != -32601 {
			t.Fatalf("expected -32601 error, got %+v", resp.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response")
	}
}

// TestMockCaller_HandleOK verifies MockCaller routes responses correctly.
func TestMockCaller_HandleOK(t *testing.T) {
	m := NewMockCaller()
	m.HandleOK("initialize", map[string]any{"capabilities": map[string]any{}})

	var result map[string]any
	if err := m.Call(context.Background(), "initialize", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if _, ok := result["capabilities"]; !ok {
		t.Fatal("expected capabilities in result")
	}
	calls := m.Calls()
	if len(calls) != 1 || calls[0].Method != "initialize" {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

// TestMockCaller_Push verifies server-initiated notifications work on MockCaller.
func TestMockCaller_Push(t *testing.T) {
	m := NewMockCaller()
	received := make(chan string, 1)
	m.SetNotificationHandler(func(method string, _ json.RawMessage) {
		received <- method
	})
	if err := m.Push("window/logMessage", map[string]any{"message": "hi"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "window/logMessage" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}
