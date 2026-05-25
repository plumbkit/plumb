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

// TestConn_Notify_SendsMessage verifies Notify writes a well-formed
// notification frame (a method, no id) on the happy path.
func TestConn_Notify_SendsMessage(t *testing.T) {
	dr, dw := io.Pipe() // conn's read side; closed at the end so readLoop exits
	defer dw.Close()
	cr, cw := io.Pipe() // conn writes here; the test reads the frame back
	conn := NewConn(dr, cw)
	defer conn.Close()

	got := make(chan wireMessage, 1)
	go func() {
		if msg, err := readMessage(bufio.NewReader(cr)); err == nil {
			got <- msg
		}
	}()

	if err := conn.Notify(context.Background(), "textDocument/didChange", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	select {
	case msg := <-got:
		if msg.Method != "textDocument/didChange" {
			t.Fatalf("method = %q, want textDocument/didChange", msg.Method)
		}
		if len(msg.ID) != 0 {
			t.Fatalf("notification carried id %q; notifications have none", msg.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for the notification frame")
	}
}

// TestConn_Notify_ContextCancelUnblocks verifies Notify returns promptly when
// the context is cancelled while the underlying write is stalled (a saturated
// language-server stdin pipe). Before the async-send fix Notify wrote
// synchronously under wrMu, so a stalled pipe blocked the caller — and thus the
// whole MCP tool call — until the server drained its buffer.
func TestConn_Notify_ContextCancelUnblocks(t *testing.T) {
	dr, dw := io.Pipe() // conn's read side; closed at the end so readLoop exits
	defer dw.Close()
	_, cw := io.Pipe() // conn writes here; no reader → the write blocks forever
	conn := NewConn(dr, cw)
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- conn.Notify(ctx, "textDocument/didChange", map[string]string{}) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Notify returned nil; want the context error when the write stalls")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Notify did not unblock after context cancel — the send is still synchronous")
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
				Result:  reqs[i].ID, // echo the raw ID as the result value
			}
			_, _ = pw.Write([]byte(frame(resp)))
		}
	}()

	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			var got int64
			if err := conn.Call(context.Background(), "echo", nil, &got); err != nil {
				t.Errorf("Call error: %v", err)
			}
		})
	}
	wg.Wait()
}

// TestConn_ContextCancel verifies a cancelled context unblocks Call.
func TestConn_ContextCancel(t *testing.T) {
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()
	defer pw.Close()
	defer cr.Close()
	go func() { _, _ = io.Copy(io.Discard, cr) }() // drain requests; never sends a response

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

// TestConn_ContextCancel_SendsCancelRequest verifies that cancelling a call's
// context emits an LSP $/cancelRequest for that request id, so the server can
// abandon the work instead of computing a result we will discard.
func TestConn_ContextCancel_SendsCancelRequest(t *testing.T) {
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()
	defer pw.Close()

	conn := NewConn(pr, cw)
	defer conn.Close()

	br := bufio.NewReader(cr)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- conn.Call(ctx, "noreply", nil, nil) }()

	// The first frame on the wire is the outbound request.
	req, err := readMessage(br)
	if err != nil {
		t.Fatalf("reading request frame: %v", err)
	}
	if req.Method != "noreply" {
		t.Fatalf("first frame method = %q, want noreply", req.Method)
	}

	cancel()

	// After cancellation the connection must emit $/cancelRequest for the same id.
	got, err := readMessage(br)
	if err != nil {
		t.Fatalf("reading cancel frame: %v", err)
	}
	if got.Method != "$/cancelRequest" {
		t.Fatalf("second frame method = %q, want $/cancelRequest", got.Method)
	}
	var p struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(got.Params, &p); err != nil {
		t.Fatalf("decoding cancel params: %v", err)
	}
	if string(p.ID) != string(req.ID) {
		t.Errorf("cancelRequest id = %s, want %s", p.ID, req.ID)
	}

	if err := <-errCh; err == nil {
		t.Fatal("expected Call to return an error after cancel")
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

	req := wireMessage{JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: "client/registerCapability", Params: json.RawMessage(`{}`)}
	_, _ = pw.Write([]byte(frame(req)))

	select {
	case resp := <-respCh:
		if string(resp.ID) != "7" {
			t.Fatalf("response ID = %q, want \"7\"", string(resp.ID))
		}
		if resp.Error != nil {
			t.Fatalf("unexpected error response: %v", resp.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response")
	}
}

// TestConn_ServerRequest_StringID verifies that server-initiated requests with
// a string ID (e.g. jdtls sends client/registerCapability with "id":"1") are
// handled correctly and do not break the read loop.
// Regression: ID was decoded as *int64 which failed for string values, killing
// the read loop and preventing any subsequent messages from being received.
func TestConn_ServerRequest_StringID(t *testing.T) {
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()

	conn := NewConn(pr, cw)
	defer conn.Close()

	conn.SetRequestHandler(func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "client/registerCapability" {
			return nil, nil
		}
		return nil, &MethodNotFoundError{Method: method}
	})

	respCh := make(chan wireMessage, 1)
	go func() {
		msg, err := readMessage(bufio.NewReader(cr))
		if err == nil {
			respCh <- msg
		}
	}()

	// jdtls sends client/registerCapability with a string ID.
	req := wireMessage{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "client/registerCapability", Params: json.RawMessage(`{}`)}
	_, _ = pw.Write([]byte(frame(req)))

	select {
	case resp := <-respCh:
		if string(resp.ID) != `"1"` {
			t.Fatalf("response ID = %q, want \"\\\"1\\\"\"", string(resp.ID))
		}
		if resp.Error != nil {
			t.Fatalf("unexpected error response: %v", resp.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response — read loop may have died on string ID")
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

	req := wireMessage{JSONRPC: "2.0", ID: json.RawMessage(`11`), Method: "weird/thing"}
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
